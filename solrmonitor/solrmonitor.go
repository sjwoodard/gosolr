// Copyright 2016 FullStory, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package solrmonitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/samuel/go-zookeeper/zk"
)

// Keeps an in-memory copy of the current state of the Solr cluster; automatically updates on ZK changes.
type SolrMonitor struct {
	mu          sync.RWMutex
	logger      zk.Logger              // where to debug log
	zkCli       ZkCli                  // the ZK client
	solrRoot    string                 // e.g. "/solr"
	collections map[string]*collection // map of all currently-known collections
	liveNodes   []string               // current set of live_nodes

	zkWatcher *zkWatcherMan
}

// Minimal interface solrmonitor needs (allows for mock ZK implementations).
type ZkCli interface {
	ChildrenW(path string) ([]string, *zk.Stat, <-chan zk.Event, error)
	Get(path string) ([]byte, *zk.Stat, error)
	GetW(path string) ([]byte, *zk.Stat, <-chan zk.Event, error)
	ExistsW(path string) (bool, *zk.Stat, <-chan zk.Event, error)
	State() zk.State
}

func NewSolrMonitor(zkCli ZkCli) (*SolrMonitor, error) {
	return NewSolrMonitorWithLogger(zkCli, zk.DefaultLogger)
}

func NewSolrMonitorWithLogger(zkCli ZkCli, logger zk.Logger) (*SolrMonitor, error) {
	return NewSolrMonitorWithRoot(zkCli, logger, "/solr")
}

func NewSolrMonitorWithRoot(zkCli ZkCli, logger zk.Logger, solrRoot string) (*SolrMonitor, error) {
	c := &SolrMonitor{
		logger:      logger,
		zkCli:       zkCli,
		solrRoot:    solrRoot,
		collections: make(map[string]*collection),
		zkWatcher:   NewZkWatcherMan(logger, zkCli),
	}
	err := c.start()
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *SolrMonitor) Close() {
	c.zkWatcher.Close()
}

func (c *SolrMonitor) GetCurrentState() (ClusterState, error) {
	if c.zkCli.State() != zk.StateHasSession {
		return nil, errors.New("not currently connected to zk")
	}
	result := make(ClusterState)

	c.mu.RLock()
	defer c.mu.RUnlock()
	for name := range c.collections {
		collectionState, err := c.doGetCollectionState(name)
		if err != nil {
			c.logger.Printf("ERROR: fetching state for collection %s: %s", name, err)
			continue
		}
		if collectionState != nil {
			result[name] = collectionState
		}
	}

	return result, nil
}

func (c *SolrMonitor) GetCollectionState(name string) (*CollectionState, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.doGetCollectionState(name)
}

func (c *SolrMonitor) doGetCollectionState(name string) (*CollectionState, error) {
	coll := c.collections[name]
	if coll == nil {
		return nil, nil
	}
	return coll.GetData()
}

func (c *SolrMonitor) GetLiveNodes() ([]string, error) {
	if c.zkCli.State() != zk.StateHasSession {
		return nil, errors.New("not currently connected to zk")
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string{}, c.liveNodes...), nil
}

func (c *SolrMonitor) start() error {
	// Synchronously check the initial calls, then setup event listening.

	// Ensure no state format v1 collections exist.
	globalClusterStatePath := c.solrRoot + "/clusterstate.json"
	globalClusterState, _, err := c.zkCli.Get(globalClusterStatePath)
	if err != nil {
		c.logger.Printf("%s: error fetching zk node: %s", globalClusterStatePath, err)
		return err
	}
	if len(globalClusterState) > 2 {
		return fmt.Errorf("%s: solrmonitor does not support state format v1; zk node should contain only '{}'; please use Solr's MIGRATESTATEFORMAT collections command", globalClusterStatePath)
	}

	collectionsPath := c.solrRoot + "/collections"
	isInit := true
	err = c.zkWatcher.monitorChildren(true, collectionsPath, func(children []string) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.updateCollections(children, isInit)
		return true
	})
	if err != nil {
		c.logger.Printf("%s: error getting children: %s", collectionsPath, err)
		return err
	}
	isInit = false

	liveNodesPath := c.solrRoot + "/live_nodes"
	c.zkWatcher.monitorChildren(true, liveNodesPath, func(children []string) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.updateLiveNodes(children)
		return true
	})
	if err != nil {
		c.logger.Printf("%s: error getting children: %s", liveNodesPath, err)
		return err
	}
	return nil
}

// Update the set of active collections; must hold the lock except during init().
func (c *SolrMonitor) updateCollections(collections []string, isInit bool) {
	c.logger.Printf("solrmonitor collections: %s", collections)
	collectionExists := make(map[string]bool)

	// First, add any collections that don't already exist
	var wg sync.WaitGroup
	if isInit {
		defer wg.Wait()
	}
	for _, name := range collections {
		collectionExists[name] = true
		_, found := c.collections[name]
		if !found {
			coll := &collection{
				name:          name,
				parent:        c,
				zkNodeVersion: -1,
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				coll.start(isInit)
			}()
			c.collections[name] = coll
		}
	}

	// Now remove any collections that disappeared.
	for name, coll := range c.collections {
		if !collectionExists[name] {
			coll.close()
			delete(c.collections, name)
		}
	}
}

func (c *SolrMonitor) updateLiveNodes(liveNodes []string) {
	c.logger.Printf("live_nodes (%d): %s", len(liveNodes), liveNodes)
	c.liveNodes = liveNodes
}

// Represents an individual collection.
type collection struct {
	mu            sync.RWMutex
	name          string       // the name of the collection
	stateData     string       // the current state.json data, or empty if no state.json node exists
	zkNodeVersion int32        // the version of the state.json data, or -1 if no state.json node exists
	parent        *SolrMonitor // if nil, this collection object was removed from the ClusterState
	isClosed      bool

	cachedState *parsedCollectionState // cached of the current parsed stateData if non-nil
}

type parsedCollectionState struct {
	collectionState *CollectionState // if non-nil, the parsed stateData
	err             error            // if non-nil, the error generated parsing stateData
}

// Returns the current collection state data.
func (coll *collection) GetData() (*CollectionState, error) {
	cachedState := func() *parsedCollectionState {
		coll.mu.RLock()
		defer coll.mu.RUnlock()
		return coll.cachedState
	}()

	if cachedState == nil {
		cachedState = func() *parsedCollectionState {
			coll.mu.Lock()
			defer coll.mu.Unlock()

			// Could have been initialized in between our first read and getting the write lock.
			if coll.cachedState == nil {
				coll.cachedState = parseStateData(coll.name, []byte(coll.stateData), coll.zkNodeVersion)
			}

			return coll.cachedState
		}()
	}

	return cachedState.collectionState, cachedState.err
}

func parseStateData(name string, data []byte, version int32) *parsedCollectionState {
	if len(data) == 0 {
		return &parsedCollectionState{}
	}
	// The individual per-collection state.json files are kind of weird; they are a single-element map from collection
	// name to a CollectionState object.  In other words they are a ClusterState, containing only the relevant collection.
	var state ClusterState
	if err := json.Unmarshal(data, &state); err != nil {
		return &parsedCollectionState{err: err}
	}

	var keys []string
	for k := range state {
		keys = append(keys, k)
	}

	if len(keys) != 1 || keys[0] != name {
		err := fmt.Errorf("Expected 1 key, got %s", keys)
		return &parsedCollectionState{err: err}
	}

	collState := state[name]
	collState.ZkNodeVersion = version
	return &parsedCollectionState{collectionState: collState}
}

func (coll *collection) start(isInit bool) {
	path := coll.parent.solrRoot + "/collections/" + coll.name + "/state.json"
	coll.parent.zkWatcher.monitorData(isInit, path, func(data string, version int32) bool {
		coll.mu.Lock()
		defer coll.mu.Unlock()
		coll.setData(data, version)
		return !coll.isClosed
	})
}

func (coll *collection) close() {
	coll.mu.Lock()
	defer coll.mu.Unlock()
	coll.isClosed = true
}

func (coll *collection) IsClosed() bool {
	coll.mu.RLock()
	defer coll.mu.RUnlock()
	return coll.isClosed
}

func (coll *collection) setData(data string, version int32) {
	if data == "" {
		coll.parent.logger.Printf("%s: no data", coll.name)
	}
	coll.stateData = data
	coll.zkNodeVersion = version
	coll.cachedState = nil
}
