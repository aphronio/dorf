package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

// CopyPersistenceCapture copies only the guarded roots, preserving their restore
// layout. The caller must finish the native guard before accepting this copy.
// No hard links to live files are allowed: execution resumes during upload.
func (a Agent) CopyPersistenceCapture(ctx context.Context, owner provider.Ownership, capture PersistenceCapture) (string, error) {
	if err := validatePersistenceID(capture.ID); err != nil {
		return "", err
	}
	root := persistenceRoot + "/copy-" + capture.ID
	paths, err := json.Marshal(capture.Paths)
	if err != nil {
		return "", err
	}
	excludes, err := json.Marshal(capture.Excludes)
	if err != nil {
		return "", err
	}
	result, err := a.Sandbox.Run(ctx, owner, provider.RunRequest{
		Args:    []string{"python3", "-c", copyPersistenceTree, root, string(paths), string(excludes)},
		Timeout: 120 * time.Second,
	})
	if err != nil || result.ExitCode != 0 {
		return "", fmt.Errorf("copy native checkpoint failed")
	}
	return root, nil
}

func (a Agent) RemovePersistenceCopy(ctx context.Context, owner provider.Ownership, capture PersistenceCapture) error {
	if err := validatePersistenceID(capture.ID); err != nil {
		return err
	}
	result, err := a.Sandbox.Exec(ctx, owner, nil, "rm", "-rf", "--", persistenceRoot+"/copy-"+capture.ID)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("remove native checkpoint copy failed")
	}
	return nil
}

const copyPersistenceTree = `import fnmatch,json,os,shutil,sys
root=sys.argv[1]
paths=json.loads(sys.argv[2])
excludes=json.loads(sys.argv[3])
os.umask(0o077)
os.mkdir(root,0o700)
def ignored(directory,names):
    return [name for name in names if any(directory==os.path.dirname(pattern) and fnmatch.fnmatchcase(name,os.path.basename(pattern)) for pattern in excludes)]
for source in paths:
    target=root+source
    os.makedirs(os.path.dirname(target),exist_ok=True)
    shutil.copytree(source,target,symlinks=True,ignore=ignored,copy_function=shutil.copy2)
    parent=os.path.dirname(source)
    while parent!='/':
        shutil.copystat(parent,root+parent,follow_symlinks=False)
        parent=os.path.dirname(parent)
`
