package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

const (
	persistenceRoot         = "/var/tmp/dorf-persistence"
	persistenceCodexHome    = "/root/.codex"
	persistenceCodexVersion = "0.154.0"
	persistenceWatchSeconds = "1830"
	persistenceErrorPrefix  = "dorf-persistence-error:"
)

// PersistenceCaptureError is a safe, stable classification of native capture
// setup failures. It deliberately omits guest paths and process diagnostics so
// callers may attach Class to operational telemetry.
type PersistenceCaptureError struct {
	Class string
}

func (e *PersistenceCaptureError) Error() string {
	return "Codex persistence capture failed: " + e.Class
}

const (
	PersistenceUnsupportedVersion   = "unsupported_version"
	PersistenceNativeMissingState   = "native_missing_state"
	PersistenceSourceChangedSetup   = "source_changed_setup"
	PersistenceSourceChangedCapture = "source_changed_capture"
	PersistenceWatcherUnavailable   = "watcher_unavailable"
)

// PersistenceCapture is the guest-side proof interval for one external backup.
// Paths contains the provider workspace, explicit extra directories and Codex
// home, including client configuration and file-based credentials. Excludes is
// the adapter-owned selection shared by the native guard and backup transport.
type PersistenceCapture struct {
	ID       string
	Paths    []string
	Excludes []string
}

// BeginPersistenceCapture proves the retained native Turns are settled and
// starts observing every mutation below the workspace, configured paths, and
// Codex home before it returns paths and exclusions to the backup driver.
// The app-server remains running.
//
// This guard does not close the durable admission-versus-publication race. The
// caller must separately invalidate the attempt when new work is admitted and
// publish the resulting snapshot under the same durable generation boundary.
func (a Agent) BeginPersistenceCapture(ctx context.Context, owner provider.Ownership, workspace string, threadID string, extraPaths []string) (PersistenceCapture, error) {
	var runs []retainedTurn
	err := a.withServer(ctx, owner, func(p *protocol) error {
		var err error
		runs, err = a.captureContinuity(ctx, owner, p, threadID)
		return err
	})
	if err != nil {
		return PersistenceCapture{}, err
	}
	extras, err := normalizePersistenceExtraPaths(workspace, extraPaths)
	if err != nil {
		return PersistenceCapture{}, err
	}
	id, err := newPersistenceID()
	if err != nil {
		return PersistenceCapture{}, err
	}
	stateDir := persistenceRoot + "/native-" + id
	extraJSON, err := json.Marshal(extras)
	if err != nil {
		return PersistenceCapture{}, err
	}
	// One adapter-owned exclusion list governs observation and backup.
	nativeJSON, err := json.Marshal(nativePersistenceExclusions)
	if err != nil {
		return PersistenceCapture{}, err
	}
	nativeRequired := len(runs) > 0
	required := "0"
	if nativeRequired {
		required = "1"
	}
	result, err := a.Sandbox.Exec(ctx, owner, nil, "bash", "-c", startPersistenceWatcher,
		"dorf-persistence-watch", stateDir, workspace, persistenceCodexHome, persistenceWatcher, string(nativeJSON),
		persistenceCodexVersion, string(extraJSON), persistenceWatchSeconds, required)
	if err != nil {
		a.cancelPersistenceStart(ctx, owner, id)
		return PersistenceCapture{}, err
	}
	if result.ExitCode != 0 {
		a.cancelPersistenceStart(ctx, owner, id)
		return PersistenceCapture{}, persistenceStartError(result)
	}
	var paths []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &paths); err != nil {
		a.cancelPersistenceStart(ctx, owner, id)
		return PersistenceCapture{}, fmt.Errorf("decode Codex persistence paths: %w", err)
	}
	if err := validatePersistencePaths(workspace, extras, paths, nativeRequired); err != nil {
		a.cancelPersistenceStart(ctx, owner, id)
		return PersistenceCapture{}, err
	}
	// Rollout verification is inside the observed interval and reads files
	// directly. Codex 0.154 thread/read writes its paginated history database,
	// so it cannot establish readiness without invalidating the same source.
	if err := a.verifyPersistedTurns(ctx, owner, runs); err != nil {
		a.cancelPersistenceStart(ctx, owner, id)
		return PersistenceCapture{}, fmt.Errorf("verify Codex persistence readiness: %w", err)
	}
	excludes := make([]string, 0, len(nativePersistenceExclusions))
	for _, name := range nativePersistenceExclusions {
		excludes = append(excludes, persistenceCodexHome+"/"+name)
	}
	return PersistenceCapture{ID: id, Paths: paths, Excludes: excludes}, nil
}

func (a Agent) verifyPersistedTurns(ctx context.Context, owner provider.Ownership, runs []retainedTurn) error {
	if len(runs) == 0 {
		return nil
	}
	expected := make(map[string]map[string]string)
	for _, run := range runs {
		if run.ThreadID == "" || run.TurnID == "" || (run.TurnOutcome != "completed" && run.TurnOutcome != "failed" && run.TurnOutcome != "interrupted") {
			return fmt.Errorf("persistence readiness requires exact settled native Turns")
		}
		turns := expected[run.ThreadID]
		if turns == nil {
			turns = make(map[string]string)
			expected[run.ThreadID] = turns
		}
		if observed, duplicate := turns[run.TurnID]; duplicate && observed != run.TurnOutcome {
			return fmt.Errorf("persistence readiness has conflicting native Turn outcomes")
		}
		turns[run.TurnID] = run.TurnOutcome
	}
	encoded, err := json.Marshal(expected)
	if err != nil {
		return fmt.Errorf("encode persistence readiness: %w", err)
	}
	result, err := a.Sandbox.Exec(ctx, owner, nil, "python3", "-c", verifyPersistedTurns, persistenceCodexHome, string(encoded))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("persisted Codex history does not match exact settled Turns")
	}
	return nil
}

func persistenceStartError(result provider.Result) error {
	class := ""
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.HasPrefix(line, persistenceErrorPrefix) {
			class = strings.TrimPrefix(line, persistenceErrorPrefix)
			break
		}
	}
	switch class {
	case PersistenceUnsupportedVersion, PersistenceNativeMissingState,
		PersistenceSourceChangedSetup, PersistenceWatcherUnavailable:
	default:
		class = PersistenceWatcherUnavailable
	}
	return &PersistenceCaptureError{Class: class}
}

func (a Agent) cancelPersistenceStart(ctx context.Context, owner provider.Ownership, id string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = a.CancelPersistenceCapture(cleanupCtx, owner, PersistenceCapture{ID: id})
}

// FinishPersistenceCapture stops observation after the backup command has
// completed and succeeds only when the protected tree stayed unchanged for the
// entire interval. A clean result is publication evidence only when combined
// with the caller's durable admission generation check.
func (a Agent) FinishPersistenceCapture(ctx context.Context, owner provider.Ownership, capture PersistenceCapture) error {
	if err := validatePersistenceID(capture.ID); err != nil {
		return err
	}
	result, err := a.Sandbox.Exec(ctx, owner, nil, "bash", "-c", finishPersistenceWatcher,
		"dorf-persistence-finish", persistenceRoot+"/native-"+capture.ID)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 && strings.TrimSpace(result.Stdout) == "clean" {
		return nil
	}
	if result.ExitCode == 24 {
		return &PersistenceCaptureError{Class: PersistenceSourceChangedCapture}
	}
	return &PersistenceCaptureError{Class: PersistenceWatcherUnavailable}
}

// CancelPersistenceCapture requests watcher exit and confirms bounded process
// termination before removing transient control files. This runs on checkpoint capacity; foreground delivery
// must never wait for guest-side checkpoint cleanup.
func (a Agent) CancelPersistenceCapture(ctx context.Context, owner provider.Ownership, capture PersistenceCapture) error {
	if err := validatePersistenceID(capture.ID); err != nil {
		return err
	}
	result, err := a.Sandbox.Exec(ctx, owner, nil, "bash", "-c", cancelPersistenceWatcher,
		"dorf-persistence-cancel", persistenceRoot+"/native-"+capture.ID)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("cancel Codex persistence guard (exit %d)", result.ExitCode)
	}
	return nil
}

func newPersistenceID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create persistence attempt identity: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func validatePersistenceID(id string) error {
	if len(id) != 32 {
		return fmt.Errorf("invalid Codex persistence attempt identity")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return fmt.Errorf("invalid Codex persistence attempt identity")
	}
	return nil
}

func normalizePersistenceExtraPaths(workspace string, paths []string) ([]string, error) {
	workspace = filepath.Clean(workspace)
	if !filepath.IsAbs(workspace) || workspace == "/" {
		return nil, fmt.Errorf("Codex persistence requires a bounded absolute workspace")
	}
	cleaned := make([]string, 0, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if path != clean || !filepath.IsAbs(clean) || clean == "/" {
			return nil, fmt.Errorf("additional persistence paths must be clean bounded absolute directories")
		}
		for _, protected := range append([]string{workspace, persistenceCodexHome, persistenceRoot, "/root/.config/dorf"}, cleaned...) {
			if pathContains(clean, protected) || pathContains(protected, clean) {
				return nil, fmt.Errorf("additional persistence paths must be disjoint from managed state")
			}
		}
		cleaned = append(cleaned, clean)
	}
	return cleaned, nil
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validatePersistencePaths(workspace string, extras, paths []string, nativeRequired bool) error {
	cleanWorkspace := filepath.Clean(workspace)
	if !filepath.IsAbs(cleanWorkspace) || cleanWorkspace == "/" || len(paths) < len(extras)+1 || paths[0] != cleanWorkspace {
		return fmt.Errorf("Codex persistence guard returned invalid protected paths")
	}
	for index, extra := range extras {
		if paths[index+1] != extra {
			return fmt.Errorf("Codex persistence guard returned invalid protected paths")
		}
	}
	native := paths[len(extras)+1:]
	if len(native) == 0 && !nativeRequired {
		return nil
	}
	if len(native) != 1 || native[0] != persistenceCodexHome {
		return fmt.Errorf("Codex persistence guard omitted native home")
	}
	return nil
}

// Top-level basenames only: the guard matches these names and restic receives
// the same patterns anchored to Codex home. WAL files remain protected.
var nativePersistenceExclusions = []string{
	"log", "logs_2.sqlite", "logs_2.sqlite-wal",
	"*.sqlite-shm", "shell_snapshots",
}

const startPersistenceWatcher = `set -eu
state=$1
workspace=$2
codex_home=$3
watcher=$4
exclusions=$5
version=$6
extras=$7
watch_seconds=$8
native_required=$9
observed_version=$(codex --version 2>/dev/null) || {
  printf '` + persistenceErrorPrefix + PersistenceUnsupportedVersion + `\n'
  exit 21
}
test "$observed_version" = "codex-cli $version" || {
  printf '` + persistenceErrorPrefix + PersistenceUnsupportedVersion + `\n'
  exit 21
}
umask 077
install -d -m 700 "` + persistenceRoot + `"
test ! -e "$state"
mkdir -m 700 "$state"
nohup python3 -c "$watcher" "$state" "$workspace" "$codex_home" "$exclusions" "$extras" "$watch_seconds" "$native_required" </dev/null >"$state/stdout" 2>"$state/stderr" &
report_error() {
  class=` + PersistenceWatcherUnavailable + `
  if IFS= read -r observed < "$state/error"; then
    case "$observed" in
      ` + PersistenceNativeMissingState + `|` + PersistenceSourceChangedSetup + `|` + PersistenceWatcherUnavailable + `) class=$observed ;;
    esac
  fi
  printf '` + persistenceErrorPrefix + `%s\n' "$class"
  exit 22
}
i=0
while test ! -e "$state/ready"; do
  test ! -e "$state/error" || report_error
  i=$((i+1)); test "$i" -lt 600 || exit 23
  sleep 0.05
done
test ! -e "$state/error" || report_error
cat "$state/paths.json"
`

const finishPersistenceWatcher = `set -eu
state=$1
test -d "$state"
: > "$state/finish.new"
mv -f "$state/finish.new" "$state/finish"
i=0
while test ! -e "$state/status"; do
  test ! -e "$state/error" || exit 22
  i=$((i+1)); test "$i" -lt 1200 || exit 23
  sleep 0.05
done
status=$(cat "$state/status")
if test "$status" = clean; then
  printf 'clean\n'
  exit 0
fi
exit 24
`

const cancelPersistenceWatcher = `set -eu
state=$1
test ! -d "$state" && exit 0
: > "$state/cancel.new"
mv -f "$state/cancel.new" "$state/cancel"
i=0
while test "$i" -lt 40; do
  test -e "$state/pid" || { sleep 0.025; i=$((i+1)); continue; }
  IFS= read -r pid < "$state/pid"
  IFS= read -r started < "$state/started"
  test -r "/proc/$pid/stat" || { rm -rf -- "$state"; exit 0; }
  observed=$(awk '{print $22}' "/proc/$pid/stat")
  test "$observed" = "$started" || { rm -rf -- "$state"; exit 0; }
  sleep 0.025
  i=$((i+1))
done
exit 23
`

// verifyPersistedTurns mirrors Codex 0.154's terminal rollout reduction for
// the fields Dorf persists. Keep its Error, TurnAborted, TurnStarted, and
// TurnComplete handling aligned with app-server-protocol's thread_history.rs.
// It performs no app-server RPC and opens no SQLite database, so readiness
// itself cannot change the protected capture source.
const verifyPersistedTurns = `import json,os,sys
home=sys.argv[1]
expected=json.loads(sys.argv[2])
found={}
def error_affects_turn(payload):
    info=payload.get('codex_error_info')
    if info=='thread_rollback_failed': return False
    if isinstance(info,dict) and 'active_turn_not_steerable' in info: return False
    return True
for dirname in ('sessions','archived_sessions'):
    root=os.path.join(home,dirname)
    if not os.path.isdir(root): continue
    for directory,subdirs,files in os.walk(root,followlinks=False):
        subdirs.sort(key=os.fsencode)
        for name in sorted(files,key=os.fsencode):
            if not name.endswith('.jsonl'): continue
            thread=''
            statuses={}
            current=''
            try:
                source=open(os.path.join(directory,name),'r',encoding='utf-8')
                with source:
                    for line in source:
                        item=json.loads(line)
                        payload=item.get('payload') or {}
                        kind=item.get('type')
                        if kind=='session_meta':
                            thread=payload.get('id') or payload.get('session_id') or ''
                        if kind!='event_msg': continue
                        event=payload.get('type')
                        turn=payload.get('turn_id') or ''
                        if event in ('task_started','turn_started') and turn:
                            current=turn
                            statuses[turn]='in_progress'
                        elif event=='error':
                            if current and error_affects_turn(payload): statuses[current]='failed'
                        elif event in ('turn_aborted',):
                            target=turn if turn and (turn==current or turn in statuses) else current
                            if target: statuses[target]='interrupted'
                        elif event in ('task_complete','turn_complete'):
                            closes_current=False
                            if turn and turn==current:
                                target=turn
                                closes_current=True
                            elif turn and turn in statuses:
                                target=turn
                            else:
                                target=current
                                closes_current=bool(current)
                            if target:
                                if payload.get('error') is not None: statuses[target]='failed'
                                elif statuses.get(target) in ('in_progress','completed'): statuses[target]='completed'
                            if closes_current: current=''
            except (OSError,UnicodeError,json.JSONDecodeError,TypeError):
                sys.exit(2)
            if thread in expected:
                if thread in found: sys.exit(3)
                found[thread]=statuses
for thread,turns in expected.items():
    observed=found.get(thread)
    if observed is None: sys.exit(4)
    for turn,outcome in turns.items():
        if observed.get(turn)!=outcome: sys.exit(5)
`

// persistenceWatcher uses only the Python standard library available in the
// verified guest image. inotify records intervening writes; two setup scans
// and the final metadata digest close watcher-installation races. Operational
// files elsewhere in Codex home are ignored because they are not restored.
// It intentionally hashes metadata rather than file contents: a real write
// changes ctime and is also reported by inotify, while restic owns content I/O.
const persistenceWatcher = `import ctypes, fnmatch, hashlib, json, os, select, struct, sys, time
from pathlib import Path

state=Path(sys.argv[1])
workspace=os.path.normpath(sys.argv[2])
codex_home=os.path.normpath(sys.argv[3])
exclusions=json.loads(sys.argv[4])
extras=json.loads(sys.argv[5])
watch_seconds=float(sys.argv[6])
native_required=sys.argv[7]=='1'
IN_MODIFY=0x2
IN_ATTRIB=0x4
IN_CLOSE_WRITE=0x8
IN_MOVED_FROM=0x40
IN_MOVED_TO=0x80
IN_CREATE=0x100
IN_DELETE=0x200
IN_DELETE_SELF=0x400
IN_MOVE_SELF=0x800
IN_UNMOUNT=0x2000
IN_Q_OVERFLOW=0x4000
IN_IGNORED=0x8000
MASK=IN_MODIFY|IN_ATTRIB|IN_CLOSE_WRITE|IN_MOVED_FROM|IN_MOVED_TO|IN_CREATE|IN_DELETE|IN_DELETE_SELF|IN_MOVE_SELF|IN_UNMOUNT|IN_Q_OVERFLOW|IN_IGNORED
EVENT=struct.Struct('iIII')
libc=ctypes.CDLL(None,use_errno=True)
fd=-1
watches={}

def atomic(name,data):
    tmp=state/(name+'.new')
    tmp.write_text(data)
    os.replace(tmp,state/name)

def fail(code):
    if code not in ('native_missing_state','source_changed_setup','watcher_unavailable'):
        code='watcher_unavailable'
    atomic('error',code+'\n')

def canonical_root(path):
    if not os.path.isabs(path) or os.path.realpath(path)!=path or not os.path.isdir(path):
        raise RuntimeError('invalid_root')
    return path

def add_watch(path,scope):
    wd=libc.inotify_add_watch(fd,os.fsencode(path),MASK)
    if wd<0:
        raise OSError(ctypes.get_errno(),'inotify_add_watch')
    watches[wd]=scope

def add_record(digest,label,root,path,st):
    rel=os.path.relpath(path,root)
    target=os.readlink(path) if os.path.islink(path) else ''
    record=[label,rel,st.st_mode,st.st_dev,st.st_ino,st.st_nlink,st.st_size,st.st_mtime_ns,st.st_ctime_ns,target]
    digest.update(json.dumps(record,separators=(',',':'),ensure_ascii=False).encode('utf-8','surrogateescape'))
    digest.update(b'\n')

def excluded(name):
    return any(fnmatch.fnmatchcase(name,pattern) for pattern in exclusions)

def fingerprint(roots,codex_exists,install,abortable=False):
    digest=hashlib.sha256()
    described=[('workspace',roots[0],'workspace')]
    described.extend(('additional-'+str(index),root,'additional') for index,root in enumerate(roots[1:]))
    if codex_exists: described.append(('native',codex_home,'native'))
    for label,root,scope in described:
        stack=[root]
        while stack:
            if abortable and (state/'cancel').exists(): raise KeyboardInterrupt()
            path=stack.pop()
            try:
                st=os.lstat(path)
            except FileNotFoundError:
                digest.update(json.dumps([label,os.path.relpath(path,root),'missing'],separators=(',',':')).encode())
                digest.update(b'\n')
                continue
            if install and not os.path.islink(path) and (path==root or os.path.isdir(path)):
                add_watch(path,'native-parent' if path==codex_home else scope)
            if path==codex_home:
                # Excluded child creation changes directory timestamps and size.
                # Preserve root identity/mode; events and enumeration cover children.
                digest.update(json.dumps([label,st.st_mode,st.st_dev,st.st_ino]).encode())
            else:
                add_record(digest,label,root,path,st)
            if os.path.isdir(path) and not os.path.islink(path):
                entries=sorted(os.scandir(path),key=lambda entry:os.fsencode(entry.name),reverse=True)
                if abortable and (state/'cancel').exists(): raise KeyboardInterrupt()
                stack.extend(entry.path for entry in entries if path!=codex_home or not excluded(entry.name))
    return digest.hexdigest()

def changed_events(timeout):
    changed=False
    readable,_,_=select.select([fd],[],[],timeout)
    while readable:
        try:
            data=os.read(fd,1<<20)
        except BlockingIOError:
            break
        offset=0
        while offset+EVENT.size<=len(data):
            wd,mask,_,length=EVENT.unpack_from(data,offset)
            raw_name=data[offset+EVENT.size:offset+EVENT.size+length].rstrip(b'\0')
            offset+=EVENT.size+length
            if not mask&MASK:
                continue
            if mask&IN_Q_OVERFLOW:
                changed=True
                continue
            scope=watches.get(wd,'watcher')
            if scope=='native-parent':
                name=os.fsdecode(raw_name)
                if name and excluded(name): continue
            elif scope=='codex-home-parent':
                if raw_name and os.fsdecode(raw_name)!=os.path.basename(codex_home): continue
            changed=True
        readable,_,_=select.select([fd],[],[],0)
    return changed

try:
    roots=[canonical_root(workspace)]+[canonical_root(path) for path in extras]
    for index,left in enumerate(roots):
        for right in roots[index+1:]:
            common=os.path.commonpath([left,right])
            if common==left or common==right:
                raise RuntimeError('overlapping_roots')
    fd=libc.inotify_init1(os.O_CLOEXEC|os.O_NONBLOCK)
    if fd<0:
        raise OSError(ctypes.get_errno(),'inotify_init1')
    atomic('pid',str(os.getpid())+'\n')
    atomic('started',Path('/proc/self/stat').read_text().split()[21]+'\n')
    codex_present=os.path.lexists(codex_home)
    codex_exists=os.path.isdir(codex_home) and os.path.realpath(codex_home)==codex_home
    if codex_present and not codex_exists: raise RuntimeError('invalid_native_state')
    if native_required and not codex_exists: raise RuntimeError('missing_native_state')
    if codex_exists:
        for entry in os.scandir(codex_home):
            if not excluded(entry.name) and entry.name.endswith(('.sqlite','.sqlite-wal')) and not entry.is_file(follow_symlinks=False):
                raise RuntimeError('invalid_native_state')
    if native_required:
        sessions=os.path.join(codex_home,'sessions')
        database=os.path.join(codex_home,'state_5.sqlite')
        if not os.path.isdir(sessions) or os.path.islink(sessions) or not os.path.isfile(database) or os.path.islink(database):
            raise RuntimeError('missing_native_state')
    if not codex_exists: add_watch(os.path.dirname(codex_home),'codex-home-parent')
    first=fingerprint(roots,codex_exists,True,True)
    changed=changed_events(0)
    baseline=fingerprint(roots,codex_exists,False,True)
    if changed_events(0) or changed or baseline!=first:
        raise RuntimeError('changed_during_setup')
    paths=[workspace]+extras
    if codex_exists: paths.append(codex_home)
    atomic('paths.json',json.dumps(paths,separators=(',',':'))+'\n')
    atomic('ready','ready\n')
    dirty=False
    deadline=time.monotonic()+watch_seconds
    while True:
        dirty=changed_events(0.05) or dirty
        if (state/'cancel').exists():
            atomic('status','canceled\n')
            break
        if (state/'finish').exists():
            final=fingerprint(roots,codex_exists,False,True)
            dirty=changed_events(0) or dirty or final!=baseline
            atomic('status',('dirty' if dirty else 'clean')+'\n')
            break
        if time.monotonic()>=deadline:
            atomic('status','expired\n')
            break
except KeyboardInterrupt:
    atomic('status','canceled\n')
except RuntimeError as exc:
    code=str(exc)
    if code in ('invalid_root','overlapping_roots','invalid_native_state','missing_native_state'):
        fail('native_missing_state')
    elif code == 'changed_during_setup':
        fail('source_changed_setup')
    else:
        fail('watcher_unavailable')
except (FileNotFoundError, NotADirectoryError):
    fail('source_changed_setup')
except Exception:
    fail('watcher_unavailable')
finally:
    if fd>=0: os.close(fd)
`
