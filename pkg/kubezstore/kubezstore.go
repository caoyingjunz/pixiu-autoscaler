/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubezstore

import "sync"

type SafeStoreInterface interface {
	Add(key string, method string, obj interface{})
	Update(key string, method string, obj interface{})
	Delete(key string)
	Get(key string) (interface{}, string, bool)
}

type SafeStore struct {
	lock    sync.RWMutex
	items   map[string]interface{}
	methods map[string]string
}

func (s *SafeStore) Get(key string) (interface{}, string, bool) {
	s.lock.Lock()
	defer s.lock.Unlock()
	item, exists1 := s.items[key]
	method, exists2 := s.methods[key]
	return item, method, exists1 && exists2
}

func (s *SafeStore) Add(key string, method string, obj interface{}) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.items[key] = obj
	s.methods[key] = method
}

func (s *SafeStore) Update(key string, method string, obj interface{}) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.items[key] = obj
	s.methods[key] = method
}

func (s *SafeStore) Delete(key string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if _, ok := s.items[key]; ok {
		delete(s.items, key)
	}
	if _, mok := s.methods[key]; mok {
		delete(s.methods, key)
	}
}

func NewSafeStore() SafeStoreInterface {
	return &SafeStore{
		items:   map[string]interface{}{},
		methods: map[string]string{},
	}
}
