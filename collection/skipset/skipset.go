// Copyright 2021 ByteDance Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package skipset is a high-performance, scalable, concurrent-safe set based on skip-list.
// In the typical pattern(100000 operations, 90%CONTAINS 9%Add 1%Remove, 8C16T), the skipset
// up to 15x faster than the built-in sync.Map.
package skipset

import (
	"github.com/bytedance/gopkg/internal/constraint"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Set represents a set based on skip list in ascending order.
type Set[K constraint.Number] struct {
	header       *node[K]
	length       int64
	highestLevel int64 // highest level for now
}

type node[K constraint.Number] struct {
	value K
	next  optionalArray // [level]*node[K
	mu    sync.Mutex
	flags bitflag
	level uint32
}

func newNode[K constraint.Number](value K, level int) *node[K] {
	node := &node[K]{
		value: value,
		level: uint32(level),
	}
	if level > op1 {
		node.next.extra = new([op2]unsafe.Pointer)
	}
	return node
}

func (n *node[K]) loadNext(i int) *node[K] {
	return (*node[K])(n.next.load(i))
}

func (n *node[K]) storeNext(i int, node *node[K]) {
	n.next.store(i, unsafe.Pointer(node))
}

func (n *node[K]) atomicLoadNext(i int) *node[K] {
	return (*node[K])(n.next.atomicLoad(i))
}

func (n *node[K]) atomicStoreNext(i int, node *node[K]) {
	n.next.atomicStore(i, unsafe.Pointer(node))
}

func (n *node[K]) lessthan(value K) bool {
	return n.value < value
}

func (n *node[K]) equal(value K) bool {
	return n.value == value
}

// NewInt64 return an empty int64 skip set in ascending order.
func New[K constraint.Number]() *Set[K] {
	h := newNode[K](0, maxLevel)
	h.flags.SetTrue(fullyLinked)
	return &Set[K]{
		header:       h,
		highestLevel: defaultHighestLevel,
	}
}

// findNodeRemove takes a value and two maximal-height arrays then searches exactly as in a sequential skip-list.
// The returned preds and succs always satisfy preds[i] > value >= succs[i].
func (s *Set[K]) findNodeRemove(value K, preds *[maxLevel]*node[K], succs *[maxLevel]*node[K]) int {
	// lFound represents the index of the first layer at which it found a node.
	lFound, x := -1, s.header
	for i := int(atomic.LoadInt64(&s.highestLevel)) - 1; i >= 0; i-- {
		succ := x.atomicLoadNext(i)
		for succ != nil && succ.lessthan(value) {
			x = succ
			succ = x.atomicLoadNext(i)
		}
		preds[i] = x
		succs[i] = succ

		// Check if the value already in the skip list.
		if lFound == -1 && succ != nil && succ.equal(value) {
			lFound = i
		}
	}
	return lFound
}

// findNodeAdd takes a value and two maximal-height arrays then searches exactly as in a sequential skip-set.
// The returned preds and succs always satisfy preds[i] > value >= succs[i].
func (s *Set[K]) findNodeAdd(value K, preds *[maxLevel]*node[K], succs *[maxLevel]*node[K]) int {
	x := s.header
	for i := int(atomic.LoadInt64(&s.highestLevel)) - 1; i >= 0; i-- {
		succ := x.atomicLoadNext(i)
		for succ != nil && succ.lessthan(value) {
			x = succ
			succ = x.atomicLoadNext(i)
		}
		preds[i] = x
		succs[i] = succ

		// Check if the value already in the skip list.
		if succ != nil && succ.equal(value) {
			return i
		}
	}
	return -1
}

func unlock[K constraint.Number](preds [maxLevel]*node[K], highestLevel int) {
	var prevPred *node[K]
	for i := highestLevel; i >= 0; i-- {
		if preds[i] != prevPred { // the node could be unlocked by previous loop
			preds[i].mu.Unlock()
			prevPred = preds[i]
		}
	}
}

// Add add the value into skip set, return true if this process insert the value into skip set,
// return false if this process can't insert this value, because another process has insert the same value.
//
// If the value is in the skip set but not fully linked, this process will wait until it is.
func (s *Set[K]) Add(value K) bool {
	level := s.randomlevel()
	var preds, succs [maxLevel]*node[K]
	for {
		lFound := s.findNodeAdd(value, &preds, &succs)
		if lFound != -1 { // indicating the value is already in the skip-list
			nodeFound := succs[lFound]
			if !nodeFound.flags.Get(marked) {
				for !nodeFound.flags.Get(fullyLinked) {
					// The node is not yet fully linked, just waits until it is.
				}
				return false
			}
			// If the node is marked, represents some other thread is in the process of deleting this node,
			// we need to add this node in next loop.
			continue
		}
		// Add this node into skip list.
		var (
			highestLocked        = -1 // the highest level being locked by this process
			valid                = true
			pred, succ, prevPred *node[K]
		)
		for layer := 0; valid && layer < level; layer++ {
			pred = preds[layer]   // target node's previous node
			succ = succs[layer]   // target node's next node
			if pred != prevPred { // the node in this layer could be locked by previous loop
				pred.mu.Lock()
				highestLocked = layer
				prevPred = pred
			}
			// valid check if there is another node has inserted into the skip list in this layer during this process.
			// It is valid if:
			// 1. The previous node and next node both are not marked.
			// 2. The previous node's next node is succ in this layer.
			valid = !pred.flags.Get(marked) && (succ == nil || !succ.flags.Get(marked)) && pred.loadNext(layer) == succ
		}
		if !valid {
			unlock[K](preds, highestLocked)
			continue
		}

		nn := newNode[K](value, level)
		for layer := 0; layer < level; layer++ {
			nn.storeNext(layer, succs[layer])
			preds[layer].atomicStoreNext(layer, nn)
		}
		nn.flags.SetTrue(fullyLinked)
		unlock[K](preds, highestLocked)
		atomic.AddInt64(&s.length, 1)
		return true
	}
}

func (s *Set[K]) randomlevel() int {
	// Generate random level.
	level := randomLevel()
	// Update highest level if possible.
	for {
		hl := atomic.LoadInt64(&s.highestLevel)
		if int64(level) <= hl {
			break
		}
		if atomic.CompareAndSwapInt64(&s.highestLevel, hl, int64(level)) {
			break
		}
	}
	return level
}

// Contains check if the value is in the skip set.
func (s *Set[K]) Contains(value K) bool {
	x := s.header
	for i := int(atomic.LoadInt64(&s.highestLevel)) - 1; i >= 0; i-- {
		nex := x.atomicLoadNext(i)
		for nex != nil && nex.lessthan(value) {
			x = nex
			nex = x.atomicLoadNext(i)
		}

		// Check if the value already in the skip list.
		if nex != nil && nex.equal(value) {
			return nex.flags.MGet(fullyLinked|marked, fullyLinked)
		}
	}
	return false
}

// Remove a node from the skip set.
func (s *Set[K]) Remove(value K) bool {
	var (
		nodeToRemove *node[K]
		isMarked     bool // represents if this operation mark the node
		topLayer     = -1
		preds, succs [maxLevel]*node[K]
	)
	for {
		lFound := s.findNodeRemove(value, &preds, &succs)
		if isMarked || // this process mark this node or we can find this node in the skip list
			lFound != -1 && succs[lFound].flags.MGet(fullyLinked|marked, fullyLinked) && (int(succs[lFound].level)-1) == lFound {
			if !isMarked { // we don't mark this node for now
				nodeToRemove = succs[lFound]
				topLayer = lFound
				nodeToRemove.mu.Lock()
				if nodeToRemove.flags.Get(marked) {
					// The node is marked by another process,
					// the physical deletion will be accomplished by another process.
					nodeToRemove.mu.Unlock()
					return false
				}
				nodeToRemove.flags.SetTrue(marked)
				isMarked = true
			}
			// Accomplish the physical deletion.
			var (
				highestLocked        = -1 // the highest level being locked by this process
				valid                = true
				pred, succ, prevPred *node[K]
			)
			for layer := 0; valid && (layer <= topLayer); layer++ {
				pred, succ = preds[layer], succs[layer]
				if pred != prevPred { // the node in this layer could be locked by previous loop
					pred.mu.Lock()
					highestLocked = layer
					prevPred = pred
				}
				// valid check if there is another node has inserted into the skip list in this layer
				// during this process, or the previous is removed by another process.
				// It is valid if:
				// 1. the previous node exists.
				// 2. no another node has inserted into the skip list in this layer.
				valid = !pred.flags.Get(marked) && pred.loadNext(layer) == succ
			}
			if !valid {
				unlock[K](preds, highestLocked)
				continue
			}
			for i := topLayer; i >= 0; i-- {
				// Now we own the `nodeToRemove`, no other goroutine will modify it.
				// So we don't need `nodeToRemove.loadNext`
				preds[i].atomicStoreNext(i, nodeToRemove.loadNext(i))
			}
			nodeToRemove.mu.Unlock()
			unlock[K](preds, highestLocked)
			atomic.AddInt64(&s.length, -1)
			return true
		}
		return false
	}
}

// Range calls f sequentially for each value present in the skip set.
// If f returns false, range stops the iteration.
func (s *Set[K]) Range(f func(value K) bool) {
	x := s.header.atomicLoadNext(0)
	for x != nil {
		if !x.flags.MGet(fullyLinked|marked, fullyLinked) {
			x = x.atomicLoadNext(0)
			continue
		}
		if !f(x.value) {
			break
		}
		x = x.atomicLoadNext(0)
	}
}

// Len return the length of this skip set.
func (s *Set[K]) Len() int {
	return int(atomic.LoadInt64(&s.length))
}
