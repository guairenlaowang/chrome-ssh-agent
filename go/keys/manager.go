// Copyright 2017 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package keys provides APIs to manage configured keys and load them into an
// SSH agent.
package keys

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"strings"

	"github.com/gopherjs/gopherjs/js"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// ID is a unique identifier for a configured key.
type ID string

const (
	// InvalidID is a special ID that will not be assigned to any key.
	InvalidID ID = ""
)

// ConfiguredKey is a key configured for use.
type ConfiguredKey struct {
	*js.Object
	// Id is the unique ID for this key.
	ID ID `js:"id"`
	// Name is a name allocated to key.
	Name string `js:"name"`
	// Encrypted indicates if the key is encrypted and requires a passphrase
	// to load.
	Encrypted bool `js:"encrypted"`
}

// LoadedKey is a key loaded into the agent.
type LoadedKey struct {
	*js.Object
	// Type is the type of key loaded in the agent (e.g., 'ssh-rsa').
	Type string `js:"type"`
	// blob is the public key material for the loaded key.
	blob string `js:"blob"`
	// Comment is a comment for the loaded key.
	Comment string `js:"comment"`
}

// SetBlob sets the given public key material for the loaded key.
func (k *LoadedKey) SetBlob(b []byte) {
	// Store as base64-encoded string. Two simpler solutions did not appear
	// to work:
	// - Storing as a []byte resulted in data not being passed via Chrome's
	//   messaging.
	// - Casting to a string resulted in different data being read from the
	//   field.
	k.blob = base64.StdEncoding.EncodeToString(b)
}

// Blob returns the public key material for the loaded key.
func (k *LoadedKey) Blob() []byte {
	b, err := base64.StdEncoding.DecodeString(k.blob)
	if err != nil {
		log.Printf("failed to decode key blob: %v", err)
		return nil
	}

	return b
}

// ID returns the unique ID corresponding to the key.  If the ID cannot be
// determined, then InvalidID is returned.
//
// The ID for a key loaded into the agent is stored in the Comment field as
// a string in a particular format.
func (k *LoadedKey) ID() ID {
	if !strings.HasPrefix(k.Comment, commentPrefix) {
		return InvalidID
	}

	return ID(strings.TrimPrefix(k.Comment, commentPrefix))
}

// Manager provides an API for managing configured keys and loading them into
// an SSH agent.
type Manager interface {
	// Configured returns the full set of keys that are configured. The
	// callback is invoked with the result.
	Configured(callback func(keys []*ConfiguredKey, err error))

	// Add configures a new key.  name is a human-readable name describing
	// the key, and pemPrivateKey is the PEM-encoded private key.  callback
	// is invoked when complete.
	Add(name string, pemPrivateKey string, callback func(err error))

	// Remove removes the key with the specified ID.  callback is invoked
	// when complete.
	//
	// Note that it might be nice to return an error here, but
	// the underlying Chrome APIs don't make it trivial to determine
	// if the requested key was removed, or ignored because it didn't
	// exist.  This could be improved, but it doesn't seem worth it at
	// the moment.
	Remove(id ID, callback func(err error))

	// Loaded returns the full set of keys loaded into the agent. The
	// callback is invoked with the result.
	Loaded(callback func(keys []*LoadedKey, err error))

	// Load loads a new key into to the agent, using the passphrase to
	// decrypt the private key.  callback is invoked when complete.
	//
	// NOTE: Unencrypted private keys are not currently supported.
	Load(id ID, passphrase string, callback func(err error))

	// Unload unloads a key from the agent. callback is invoked when
	// complete.
	Unload(key *LoadedKey, callback func(err error))
}

// PersistentStore provides access to underlying storage.  See chrome.Storage
// for details on the methods; using this interface allows for alternate
// implementations during testing.
type PersistentStore interface {
	// Set stores new data. See chrome.Storage.Set() for details.
	Set(data map[string]interface{}, callback func(err error))

	// Get gets data from storage. See chrome.Storage.Get() for details.
	Get(callback func(data map[string]interface{}, err error))

	// Delete deletes data from storage. See chrome.Storage.Delete() for
	// details.
	Delete(keys []string, callback func(err error))
}

// NewManager returns a Manager implementation that can manage keys in the
// supplied agent, and store configured keys in the supplied storage.
func NewManager(agt agent.Agent, storage PersistentStore) Manager {
	return &manager{
		agent:   agt,
		storage: storage,
	}
}

// manager is an implementation of Manager.
type manager struct {
	agent   agent.Agent
	storage PersistentStore
}

// storedKey is the raw object stored in persistent storage for a configured
// key.
type storedKey struct {
	*js.Object
	ID            ID     `js:"id"`
	Name          string `js:"name"`
	PEMPrivateKey string `js:"pemPrivateKey"`
}

// Encrypted determines if the private key is encrypted. The Proc-Type header
// contains 'ENCRYPTED' if the key is encrypted. See RFC 1421 Section 4.6.1.1.
func (s *storedKey) Encrypted() bool {
	block, _ := pem.Decode([]byte(s.PEMPrivateKey))
	if block == nil {
		// Attempt to handle this gracefully and guess that it isn't
		// encrypted.  If the key is not properly formatted, we'll
		// complain anyways when it is loaded.
		return false
	}

	return strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED")
}

const (
	// keyPrefix is the prefix for keys stored in persistent storage.
	// The full key is of the form 'key.<id>'.
	keyPrefix = "key."
	// commentPrefix is the prefix for the comment included when a
	// configured key is loaded into the agent. The full comment is of the
	// form 'chrome-ssh-agent:<id>'.
	commentPrefix = "chrome-ssh-agent:"
)

// newStoredKey converts a key-value map (e.g., which is supplied when reading
// from persistent storage) into a storedKey.
func newStoredKey(m map[string]interface{}) *storedKey {
	o := js.Global.Get("Object").New()
	for k, v := range m {
		o.Set(k, v)
	}
	return &storedKey{Object: o}
}

// readKeys returns all the stored keys from persistent storage. callback is
// invoked with the returned keys.
func (m *manager) readKeys(callback func(keys []*storedKey, err error)) {
	m.storage.Get(func(data map[string]interface{}, err error) {
		if err != nil {
			callback(nil, fmt.Errorf("failed to read from storage: %v", err))
			return
		}

		var keys []*storedKey
		for k, v := range data {
			if !strings.HasPrefix(k, keyPrefix) {
				continue
			}

			keys = append(keys, newStoredKey(v.(map[string]interface{})))
		}
		callback(keys, nil)
	})
}

// readKey returns the key of the specified ID from persistent storage. callback
// is invoked with the returned key.
func (m *manager) readKey(id ID, callback func(key *storedKey, err error)) {
	m.readKeys(func(keys []*storedKey, err error) {
		if err != nil {
			callback(nil, fmt.Errorf("failed to read keys: %v", err))
			return
		}

		for _, k := range keys {
			if k.ID == id {
				callback(k, nil)
				return
			}
		}

		callback(nil, nil)
	})
}

// writeKey writes a new key to persistent storage.  callback is invoked when
// complete.
func (m *manager) writeKey(name string, pemPrivateKey string, callback func(err error)) {
	i, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		callback(fmt.Errorf("failed to generate new ID: %v", err))
		return
	}
	id := ID(i.String())
	storageKey := fmt.Sprintf("%s%s", keyPrefix, id)
	sk := &storedKey{Object: js.Global.Get("Object").New()}
	sk.ID = id
	sk.Name = name
	sk.PEMPrivateKey = pemPrivateKey
	data := map[string]interface{}{
		storageKey: sk,
	}
	m.storage.Set(data, func(err error) {
		callback(err)
	})
}

// removeKey removes the key with the specified ID from persistent storage.
// callback is invoked on completion.
func (m *manager) removeKey(id ID, callback func(err error)) {
	m.readKeys(func(keys []*storedKey, err error) {
		if err != nil {
			callback(fmt.Errorf("failed to enumerate keys: %v", err))
			return
		}

		var storageKeys []string
		for _, k := range keys {
			if k.ID == id {
				storageKeys = append(storageKeys, fmt.Sprintf("%s%s", keyPrefix, k.ID))
			}
		}

		m.storage.Delete(storageKeys, func(err error) {
			if err != nil {
				callback(fmt.Errorf("failed to delete keys: %v", err))
				return
			}
			callback(nil)
		})
	})
}

// Configured implements Manager.Configured.
func (m *manager) Configured(callback func(keys []*ConfiguredKey, err error)) {
	m.readKeys(func(keys []*storedKey, err error) {
		if err != nil {
			callback(nil, fmt.Errorf("failed to read keys: %v", err))
			return
		}

		var result []*ConfiguredKey
		for _, k := range keys {
			c := &ConfiguredKey{Object: js.Global.Get("Object").New()}
			c.ID = k.ID
			c.Name = k.Name
			c.Encrypted = k.Encrypted()
			result = append(result, c)
		}
		callback(result, nil)
	})
}

// Add implements Manager.Add.
func (m *manager) Add(name string, pemPrivateKey string, callback func(err error)) {
	if name == "" {
		callback(errors.New("name must not be empty"))
		return
	}

	m.writeKey(name, pemPrivateKey, func(err error) {
		callback(err)
	})
}

// Remove implements Manager.Remove.
func (m *manager) Remove(id ID, callback func(err error)) {
	m.removeKey(id, func(err error) {
		callback(err)
	})
}

// Loaded implements Manager.Loaded.
func (m *manager) Loaded(callback func(keys []*LoadedKey, err error)) {
	loaded, err := m.agent.List()
	if err != nil {
		callback(nil, fmt.Errorf("failed to list loaded keys: %v", err))
		return
	}

	var result []*LoadedKey
	for _, l := range loaded {
		k := &LoadedKey{Object: js.Global.Get("Object").New()}
		k.Type = l.Type()
		k.SetBlob(l.Marshal())
		k.Comment = l.Comment
		result = append(result, k)
	}

	callback(result, nil)
}

// Load implements Manager.Load.
func (m *manager) Load(id ID, passphrase string, callback func(err error)) {
	m.readKey(id, func(key *storedKey, err error) {
		if err != nil {
			callback(fmt.Errorf("failed to read key: %v", err))
			return
		}

		if key == nil {
			callback(fmt.Errorf("failed to find key with ID %s", id))
			return
		}

		var priv interface{}
		if key.Encrypted() {
			priv, err = ssh.ParseRawPrivateKeyWithPassphrase([]byte(key.PEMPrivateKey), []byte(passphrase))
		} else {
			priv, err = ssh.ParseRawPrivateKey([]byte(key.PEMPrivateKey))
		}
		if err != nil {
			callback(fmt.Errorf("failed to parse private key: %v", err))
			return
		}

		err = m.agent.Add(agent.AddedKey{
			PrivateKey: priv,
			Comment:    fmt.Sprintf("%s%s", commentPrefix, id),
		})
		if err != nil {
			callback(fmt.Errorf("failed to add key to agent: %v", err))
			return
		}
		callback(nil)
	})
}

// Unload implements Manager.Unload.
func (m *manager) Unload(key *LoadedKey, callback func(err error)) {
	pub := &agent.Key{
		Format: key.Type,
		Blob:   key.Blob(),
	}
	if err := m.agent.Remove(pub); err != nil {
		callback(fmt.Errorf("failed to unload key: %v", err))
		return
	}
	callback(nil)
}
