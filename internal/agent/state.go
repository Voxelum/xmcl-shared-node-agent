package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

type ActiveAssignment struct {
	AssignmentID string `json:"assignmentId"`
}

type StateStore interface {
	Result(commandID string) (controlplane.CommandResult, bool, error)
	Active(serviceID string) (ActiveAssignment, bool, error)
	Commit(commandID, serviceID string, result controlplane.CommandResult, active *ActiveAssignment) error
}

type fileState struct {
	Results map[string]controlplane.CommandResult `json:"results"`
	Active  map[string]ActiveAssignment           `json:"active"`
}

type FileStore struct {
	path string
	mu   sync.Mutex
	data fileState
}

func NewFileStore(root string) (*FileStore, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	store := &FileStore{path: filepath.Join(root, "commands.json"), data: fileState{
		Results: make(map[string]controlplane.CommandResult),
		Active:  make(map[string]ActiveAssignment),
	}}
	data, err := os.ReadFile(store.path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read command state: %w", err)
	}
	if err := json.Unmarshal(data, &store.data); err != nil {
		return nil, fmt.Errorf("decode command state: %w", err)
	}
	if store.data.Results == nil {
		store.data.Results = make(map[string]controlplane.CommandResult)
	}
	if store.data.Active == nil {
		store.data.Active = make(map[string]ActiveAssignment)
	}
	return store, nil
}

func (s *FileStore) Result(commandID string) (controlplane.CommandResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, ok := s.data.Results[commandID]
	return result, ok, nil
}

func (s *FileStore) Active(serviceID string) (ActiveAssignment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	assignment, ok := s.data.Active[serviceID]
	return assignment, ok, nil
}

func (s *FileStore) Commit(commandID, serviceID string, result controlplane.CommandResult, active *ActiveAssignment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Results[commandID] = result
	if active == nil {
		delete(s.data.Active, serviceID)
	} else {
		s.data.Active[serviceID] = *active
	}
	data, err := json.Marshal(s.data)
	if err != nil {
		return fmt.Errorf("encode command state: %w", err)
	}
	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return fmt.Errorf("write command state: %w", err)
	}
	if err := os.Rename(temp, s.path); err != nil {
		return fmt.Errorf("persist command state: %w", err)
	}
	return nil
}
