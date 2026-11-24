package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/flynn/noise"
)

// dataDir holds config, identity and paired machines. KEYBAR_HOME overrides it,
// which also lets a second copy run on the same computer for testing.
func dataDir() string {
	if d := os.Getenv("KEYBAR_HOME"); d != "" {
		return d
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "keybar")
}

// hostOS is this machine's OS as told to other machines; KEYBAR_FAKE_OS lets a
// test copy pretend to be Windows.
func hostOS() string {
	if o := os.Getenv("KEYBAR_FAKE_OS"); o != "" {
		return o
	}
	return runtime.GOOS
}

var noiseSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

// Identity is this machine's long-term key pair. Paired machines remember the
// public half; it is never sent anywhere else and never expires.
type Identity struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Private []byte `json:"private"`
	Public  []byte `json:"public"`
}

func (i *Identity) Keypair() noise.DHKey { return noise.DHKey{Private: i.Private, Public: i.Public} }

func loadIdentity() (*Identity, error) {
	p := filepath.Join(dataDir(), "identity.json")
	var id Identity
	if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &id) == nil && len(id.Private) == 32 && id.ID != "" {
		return &id, nil
	}
	kp, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 8)
	rand.Read(raw)
	host, _ := os.Hostname()
	id = Identity{ID: hex.EncodeToString(raw), Name: host, Private: kp.Private, Public: kp.Public}
	return &id, id.save()
}

func (i *Identity) save() error {
	return writePrivate(filepath.Join(dataDir(), "identity.json"), i)
}

// Peer is a paired machine.
type Peer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	OS     string `json:"os"`
	Public []byte `json:"public"`
	Addr   string `json:"addr,omitempty"` // last known host:port
}

type PeerStore struct {
	mu    sync.Mutex
	path  string
	peers map[string]*Peer
}

func loadPeers() *PeerStore {
	s := &PeerStore{path: filepath.Join(dataDir(), "peers.json"), peers: map[string]*Peer{}}
	var list []*Peer
	if b, err := os.ReadFile(s.path); err == nil && json.Unmarshal(b, &list) == nil {
		for _, p := range list {
			s.peers[p.ID] = p
		}
	}
	return s
}

func (s *PeerStore) Get(id string) *Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.peers[id]; p != nil {
		cp := *p
		return &cp
	}
	return nil
}

func (s *PeerStore) List() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// Put adds or updates a peer and saves the list.
func (s *PeerStore) Put(p Peer) {
	s.mu.Lock()
	s.peers[p.ID] = &p
	s.mu.Unlock()
	s.save()
}

func (s *PeerStore) Update(id string, fn func(*Peer)) {
	s.mu.Lock()
	p := s.peers[id]
	if p == nil {
		s.mu.Unlock()
		return
	}
	before := *p
	fn(p)
	changed := before.Name != p.Name || before.OS != p.OS || before.Addr != p.Addr
	s.mu.Unlock()
	if changed {
		s.save()
	}
}

func (s *PeerStore) Delete(id string) bool {
	s.mu.Lock()
	_, ok := s.peers[id]
	delete(s.peers, id)
	s.mu.Unlock()
	if ok {
		s.save()
	}
	return ok
}

func (s *PeerStore) save() {
	writePrivate(s.path, s.List())
}

// writePrivate writes JSON readable only by the user: these files hold keys.
func writePrivate(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
