package codex

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

const maxInstructionFileBytes = 128 << 10

type instructionHashes struct {
	agents [32]byte
	soul   [32]byte
}

type workspaceInstructions struct {
	agentsPath string
	hashes     instructionHashes
	soul       string
}

func (a Agent) readWorkspaceInstructions(ctx context.Context, owner provider.Ownership, workspace string) (*workspaceInstructions, error) {
	files := make(map[string]string, 2)
	for _, name := range []string{"AGENTS.md", "SOUL.md"} {
		contents, err := a.Sandbox.ReadFile(ctx, owner, name)
		if errors.Is(err, os.ErrNotExist) {
			contents, err = nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read Codex workspace instructions %s: %w", name, err)
		}
		if len(contents) > maxInstructionFileBytes || !utf8.Valid(contents) || strings.ContainsRune(string(contents), 0) {
			return nil, fmt.Errorf("Codex instruction file %s must be valid UTF-8 without null bytes and at most %d bytes", name, maxInstructionFileBytes)
		}
		files[name] = string(contents)
	}
	return &workspaceInstructions{
		agentsPath: filepath.Join(workspace, "AGENTS.md"),
		hashes:     instructionHashes{agents: sha256.Sum256([]byte(files["AGENTS.md"])), soul: sha256.Sum256([]byte(files["SOUL.md"]))},
		soul:       files["SOUL.md"],
	}, nil
}

func (p *protocol) injectWorkspaceInstructions(ctx context.Context, threadID string) error {
	current := p.instructions
	if current == nil {
		return nil
	}
	var previous instructionHashes
	var known bool
	if p.instructionCache != nil {
		p.instructionCache.mu.Lock()
		previous, known = p.instructionCache.instructions[threadID]
		p.instructionCache.mu.Unlock()
	}
	var sections []string
	if !p.freshThread && (!known || previous.agents != current.hashes.agents) {
		sections = append(sections, fmt.Sprintf("The workspace instruction file %q was updated (revision %x). Read it in full before answering and apply its current contents instead of the previous version. If it is missing or empty, its previous instructions no longer apply.", current.agentsPath, current.hashes.agents))
	}
	if !known || previous.soul != current.hashes.soul {
		sections = append(sections, "The following are the complete current contents of SOUL.md. They replace all previous SOUL.md instructions. An empty document means no SOUL.md instructions are configured.\n\n<SOUL.md>\n"+current.soul+"\n</SOUL.md>")
	}
	if len(sections) == 0 {
		return nil
	}
	params := map[string]any{"threadId": threadID, "items": []map[string]any{{"type": "message", "role": "developer", "content": []map[string]string{{"type": "input_text", "text": strings.Join(sections, "\n\n")}}}}}
	_, err := p.call(ctx, "thread/inject_items", params)

	return err
}

func (p *protocol) rememberWorkspaceInstructions(threadID string) {
	if p.instructions == nil || p.instructionCache == nil {
		return
	}
	p.instructionCache.mu.Lock()
	defer p.instructionCache.mu.Unlock()
	p.instructionCache.instructions[threadID] = p.instructions.hashes
}
