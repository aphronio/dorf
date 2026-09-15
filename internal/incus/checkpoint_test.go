package incus

import (
	"context"
	"errors"
	"net/http"
	"testing"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

type checkpointSDK struct {
	fakeSDKServer
	snapshot                      *api.InstanceSnapshot
	captures, restores, deletions int
	loseCapture                   bool
}

func (s *checkpointSDK) WithContext(context.Context) incusclient.InstanceServer { return s }
func (s *checkpointSDK) GetInstanceSnapshot(string, string) (*api.InstanceSnapshot, string, error) {
	if s.snapshot == nil {
		return nil, "", api.StatusErrorf(http.StatusNotFound, "absent")
	}
	return s.snapshot, "snapshot-etag", nil
}
func (s *checkpointSDK) UpdateInstanceState(_ string, request api.InstanceStatePut, _ string) (incusclient.Operation, error) {
	if request.Force {
		return nil, errors.New("checkpoint must stop cleanly")
	}
	if request.Action == "stop" {
		s.instance.StatusCode = api.Stopped
	} else {
		s.instance.StatusCode = api.Running
	}
	return &fakeOperation{}, nil
}
func (s *checkpointSDK) CreateInstanceSnapshot(_ string, request api.InstanceSnapshotsPost) (incusclient.Operation, error) {
	if s.instance.IsActive() || request.Stateful {
		return nil, errors.New("capture was not quiescent")
	}
	s.captures++
	s.snapshot = &api.InstanceSnapshot{Name: request.Name, Config: api.ConfigMap(cloneStrings(s.instance.Config))}
	if s.loseCapture {
		s.loseCapture = false
		return nil, errors.New("accepted snapshot response lost")
	}
	return &fakeOperation{}, nil
}
func (s *checkpointSDK) UpdateInstance(_ string, request api.InstancePut, _ string) (incusclient.Operation, error) {
	if s.instance.IsActive() || request.Restore == "" {
		return nil, errors.New("restore was not quiescent")
	}
	s.restores++
	return &fakeOperation{}, nil
}
func (s *checkpointSDK) DeleteInstanceSnapshot(string, string) (incusclient.Operation, error) {
	s.deletions++
	s.snapshot = nil
	return &fakeOperation{}, nil
}

func TestCheckpointRecoversLostReceiptAndRejectsForeignState(t *testing.T) {
	ctx := context.Background()
	required := map[string]string{"user.dorf.ownership_nonce": "exact-source"}
	server := &checkpointSDK{loseCapture: true}
	server.instance = api.Instance{Name: "owned", StatusCode: api.Running, InstancePut: api.InstancePut{Config: api.ConfigMap(cloneStrings(required))}}
	client := &sdkClient{server: server}
	if err := client.CaptureOwnedCheckpoint(ctx, "owned", "upgrade", required); err == nil {
		t.Fatal("lost capture acknowledgement was hidden")
	}
	if err := client.CaptureOwnedCheckpoint(ctx, "owned", "upgrade", required); err != nil {
		t.Fatal(err)
	}
	if server.captures != 1 || !server.instance.IsActive() {
		t.Fatal("capture retry duplicated the snapshot or left source stopped")
	}
	server.snapshot.Config["user.dorf.ownership_nonce"] = "foreign"
	if err := client.RestoreOwnedCheckpoint(ctx, "owned", "upgrade", required); err == nil {
		t.Fatal("foreign checkpoint restored")
	}
	if err := client.DeleteOwnedCheckpoint(ctx, "owned", "upgrade", required); err == nil {
		t.Fatal("foreign checkpoint deleted")
	}
	if server.restores != 0 || server.deletions != 0 {
		t.Fatal("foreign checkpoint was mutated")
	}
	server.snapshot.Config = api.ConfigMap(cloneStrings(required))
	if err := client.RestoreOwnedCheckpoint(ctx, "owned", "upgrade", required); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteOwnedCheckpoint(ctx, "owned", "upgrade", required); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteOwnedCheckpoint(ctx, "owned", "upgrade", required); err != nil {
		t.Fatal(err)
	}
	if server.restores != 1 || server.deletions != 1 || !server.instance.IsActive() {
		t.Fatal("restore or cleanup failed to converge")
	}
	server.instance.ExpandedDevices = api.DevicesMap{"data": {"type": "disk", "path": "/workspace"}}
	if err := client.CaptureOwnedCheckpoint(ctx, "owned", "next-upgrade", required); err == nil {
		t.Fatal("external volume coverage was assumed")
	}
	if server.captures != 1 {
		t.Fatal("unsupported storage was captured")
	}
}
