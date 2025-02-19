/*
 * Copyright 2019 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package ristretto

import (
	"sync"
	"time"
)

// TODO: Do we need this to be a separate struct from Item?
type storeItem[V any] struct {
	conflict   uint64
	value      V
	expiration time.Time
}

// store is the interface fulfilled by all hash map implementations in this
// file. Some hash map implementations are better suited for certain data
// distributions than others, so this allows us to abstract that out for use
// in Ristretto.
//
// Every store is safe for concurrent usage.
type store[V any] interface {
	// Get returns the value associated with the key parameter.
	Get(uint64, uint64) (V, bool)
	// Expiration returns the expiration time for this key.
	Expiration(uint64) time.Time
	// Set adds the key-value pair to the Map or updates the value if it's
	// already present. The key-value pair is passed as a pointer to an
	// item object.
	Set(Item[V])
	// Del deletes the key-value pair from the Map.
	Del(uint64, uint64) (uint64, V)
	// Update attempts to update the key with a new value and returns true if
	// successful.
	Update(Item[V]) (V, bool)
	// Cleanup removes items that have an expired TTL.
	Cleanup(policy policy[V], onEvict itemCallback[V])
	// Clear clears all contents of the store.
	Clear(onEvict itemCallback[V])
}

// newStore returns the default store implementation.
func newStore[V any]() store[V] {
	return newShardedMap[V]()
}

const numShards uint64 = 256

type shardedMap[V any] struct {
	shards    []*lockedMap[V]
	expiryMap *expirationMap[V]
}

func newShardedMap[V any]() *shardedMap[V] {
	sm := &shardedMap[V]{
		shards:    make([]*lockedMap[V], int(numShards)),
		expiryMap: newExpirationMap[V](),
	}
	for i := range sm.shards {
		sm.shards[i] = newLockedMap[V](sm.expiryMap)
	}
	return sm
}

func (sm *shardedMap[V]) Get(key, conflict uint64) (V, bool) {
	return sm.shards[key%numShards].get(key, conflict)
}

func (sm *shardedMap[V]) Expiration(key uint64) time.Time {
	return sm.shards[key%numShards].Expiration(key)
}

func (sm *shardedMap[V]) Set(i Item[V]) {
	// TODO: i.flag should have a invalid zero value flag for invalid items.
	sm.shards[i.Key%numShards].Set(i)
}

func (sm *shardedMap[V]) Del(key, conflict uint64) (uint64, V) {
	return sm.shards[key%numShards].Del(key, conflict)
}

func (sm *shardedMap[V]) Update(newItem Item[V]) (V, bool) {
	return sm.shards[newItem.Key%numShards].Update(newItem)
}

func (sm *shardedMap[V]) Cleanup(policy policy[V], onEvict itemCallback[V]) {
	sm.expiryMap.cleanup(sm, policy, onEvict)
}

func (sm *shardedMap[V]) Clear(onEvict itemCallback[V]) {
	for i := uint64(0); i < numShards; i++ {
		sm.shards[i].Clear(onEvict)
	}
}

type lockedMap[V any] struct {
	sync.RWMutex
	data map[uint64]storeItem[V]
	em   *expirationMap[V]
}

func newLockedMap[V any](em *expirationMap[V]) *lockedMap[V] {
	return &lockedMap[V]{
		data: make(map[uint64]storeItem[V]),
		em:   em,
	}
}

func (m *lockedMap[V]) get(key, conflict uint64) (V, bool) {
	m.RLock()
	item, ok := m.data[key]
	m.RUnlock()
	if !ok {
		var zero V
		return zero, false
	}
	if conflict != 0 && (conflict != item.conflict) {
		var zero V
		return zero, false
	}

	// Handle expired items.
	if !item.expiration.IsZero() && time.Now().After(item.expiration) {
		var zero V
		return zero, false
	}
	return item.value, true
}

func (m *lockedMap[V]) Expiration(key uint64) time.Time {
	m.RLock()
	defer m.RUnlock()
	return m.data[key].expiration
}

func (m *lockedMap[V]) Set(i Item[V]) {
	// TODO: i.flag should have a invalid zero value flag for invalid items.

	m.Lock()
	defer m.Unlock()
	item, ok := m.data[i.Key]

	if ok {
		// The item existed already. We need to check the conflict key and reject the
		// update if they do not match. Only after that the expiration map is updated.
		if i.Conflict != 0 && (i.Conflict != item.conflict) {
			return
		}
		m.em.update(i.Key, i.Conflict, item.expiration, i.Expiration)
	} else {
		// The value is not in the map already. There's no need to return anything.
		// Simply add the expiration map.
		m.em.add(i.Key, i.Conflict, i.Expiration)
	}

	m.data[i.Key] = storeItem[V]{
		conflict:   i.Conflict,
		value:      i.Value,
		expiration: i.Expiration,
	}
}

func (m *lockedMap[V]) Del(key, conflict uint64) (uint64, V) {
	m.Lock()
	item, ok := m.data[key]
	if !ok {
		m.Unlock()
		var zero V
		return 0, zero
	}
	if conflict != 0 && (conflict != item.conflict) {
		m.Unlock()
		var zero V
		return 0, zero
	}

	if !item.expiration.IsZero() {
		m.em.del(key, item.expiration)
	}

	delete(m.data, key)
	m.Unlock()
	return item.conflict, item.value
}

func (m *lockedMap[V]) Update(newItem Item[V]) (V, bool) {
	m.Lock()
	item, ok := m.data[newItem.Key]
	if !ok {
		m.Unlock()
		var zero V
		return zero, false
	}
	if newItem.Conflict != 0 && (newItem.Conflict != item.conflict) {
		m.Unlock()
		var zero V
		return zero, false
	}

	m.em.update(newItem.Key, newItem.Conflict, item.expiration, newItem.Expiration)
	m.data[newItem.Key] = storeItem[V]{
		conflict:   newItem.Conflict,
		value:      newItem.Value,
		expiration: newItem.Expiration,
	}

	m.Unlock()
	return item.value, true
}

func (m *lockedMap[V]) Clear(onEvict itemCallback[V]) {
	m.Lock()
	if onEvict != nil {
		for key, si := range m.data {
			onEvict(Item[V]{
				Key:      key,
				Conflict: si.conflict,
				Value:    si.value,
			})
		}
	}
	m.data = make(map[uint64]storeItem[V])
	m.Unlock()
}
