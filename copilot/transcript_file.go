package copilot

// A TranscriptStore backed by files, because ADR 0004 and ADR 0006 make the same
// argument one layer down: this platform has no server, and a component that
// needed one to keep state would be the first.
//
// One file per exchange, named for its ID — the same shape as a compiled collector
// config in the Fleet, and readable for the same reason. An operator looking at
// what the Copilot was told during an incident should be able to open it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// maxTranscriptIDLen bounds a filename. IDs are handles, not prose.
const maxTranscriptIDLen = 64

// FileStore keeps each exchange as one JSON document under a root directory.
type FileStore struct {
	root string
}

// NewFileStore returns a store rooted at dir. The directory is created on first
// Save rather than here, so constructing a store touches no disk and a test that
// never saves leaves nothing behind.
func NewFileStore(dir string) *FileStore { return &FileStore{root: dir} }

// Save writes the exchange atomically.
//
// Temp file then rename, because the alternative is a reader — or the next turn —
// finding a half-written transcript. A truncated document would fail to parse,
// which is the good case; the bad case is one that parses into fewer turns than
// were had.
func (s *FileStore) Save(_ context.Context, id string, c *Conversation) error {
	path, err := s.pathFor(id)
	if err != nil {
		return err
	}
	if c == nil {
		return errors.New("copilot: cannot save a nil Conversation")
	}

	// Marshal BEFORE touching the filesystem. A Conversation that will not
	// serialize should not leave a directory behind as evidence that it tried.
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("copilot: serializing transcript %q: %w", id, err)
	}

	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return fmt.Errorf("copilot: transcript store root: %w", err)
	}

	tmp, err := os.CreateTemp(s.root, ".transcript-*")
	if err != nil {
		return fmt.Errorf("copilot: staging transcript %q: %w", id, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("copilot: writing transcript %q: %w", id, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("copilot: closing transcript %q: %w", id, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("copilot: committing transcript %q: %w", id, err)
	}
	return nil
}

// Load reads an exchange back, refusing any document that does not keep authored
// text and tool-result content apart — that refusal is Conversation.UnmarshalJSON,
// and this method adds nothing to it.
func (s *FileStore) Load(_ context.Context, id string) (*Conversation, error) {
	path, err := s.pathFor(id)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", ErrNoTranscript, id)
		}
		return nil, fmt.Errorf("copilot: reading transcript %q: %w", id, err)
	}

	var c Conversation
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("copilot: transcript %q: %w", id, err)
	}
	return &c, nil
}

// pathFor turns an ID into a filename, and refuses one that could name a file
// somewhere else.
//
// An ID is a handle that may have come from an HTTP path, a CLI flag, or an issue
// number pasted by whoever is running the incident. `../../etc/something` is a
// valid-looking string and a path traversal; a leading dot hides the file from the
// operator who is meant to be able to read it. So the charset is an allowlist
// rather than a list of things to strip — stripping is what leaves the next
// unusual character to be discovered later.
func (s *FileStore) pathFor(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: empty", ErrBadTranscriptID)
	}
	if len(id) > maxTranscriptIDLen {
		return "", fmt.Errorf("%w: longer than %d characters", ErrBadTranscriptID, maxTranscriptIDLen)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return "", fmt.Errorf("%w: %q contains %q", ErrBadTranscriptID, id, r)
		}
	}
	return filepath.Join(s.root, id+".json"), nil
}
