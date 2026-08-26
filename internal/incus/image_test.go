package incus

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

type fakeImageServer struct {
	incusclient.InstanceServer
	context           context.Context
	disconnected      bool
	images            map[string]api.Image
	aliases           map[string]api.ImageAliasesEntry
	aliasETags        map[string]string
	importFingerprint string
	importBytes       []byte
	importPost        api.ImagesPost
	importArgs        incusclient.ImageCreateArgs
	createdAliases    int
	updatedAliases    int
	updateETag        string
	ignoreAliasUpdate bool
}

func (s *fakeImageServer) WithContext(ctx context.Context) incusclient.InstanceServer {
	s.context = ctx
	return s
}

func (s *fakeImageServer) Disconnect() { s.disconnected = true }

func (s *fakeImageServer) GetImage(reference string) (*api.Image, string, error) {
	image, ok := s.images[reference]
	if !ok {
		return nil, "", api.StatusErrorf(http.StatusNotFound, "image not found")
	}
	copy := image
	return &copy, "image-etag", nil
}

func (s *fakeImageServer) GetImageAlias(name string) (*api.ImageAliasesEntry, string, error) {
	alias, ok := s.aliases[name]
	if !ok {
		return nil, "", api.StatusErrorf(http.StatusNotFound, "alias not found")
	}
	copy := alias
	return &copy, s.aliasETags[name], nil
}

func (s *fakeImageServer) CreateImage(post api.ImagesPost, args *incusclient.ImageCreateArgs) (incusclient.Operation, error) {
	s.importPost = post
	s.importArgs = *args
	contents, err := io.ReadAll(args.MetaFile)
	if err != nil {
		return nil, err
	}
	s.importBytes = contents
	return &fakeOperation{onWait: func() {
		s.images[s.importFingerprint] = api.Image{Fingerprint: s.importFingerprint, Type: virtualMachineImageType}
	}}, nil
}

func (s *fakeImageServer) CreateImageAlias(post api.ImageAliasesPost) error {
	s.createdAliases++
	s.aliases[post.Name] = post.ImageAliasesEntry
	s.aliasETags[post.Name] = "created-etag"
	return nil
}

func (s *fakeImageServer) UpdateImageAlias(name string, put api.ImageAliasesEntryPut, etag string) error {
	s.updatedAliases++
	s.updateETag = etag
	if !s.ignoreAliasUpdate {
		alias := s.aliases[name]
		alias.ImageAliasesEntryPut = put
		alias.Type = virtualMachineImageType
		s.aliases[name] = alias
		s.aliasETags[name] = "updated-etag"
	}
	return nil
}

func newFakeImageServer() *fakeImageServer {
	return &fakeImageServer{
		images:     map[string]api.Image{},
		aliases:    map[string]api.ImageAliasesEntry{},
		aliasETags: map[string]string{},
	}
}

func TestSDKImageReferenceResolutionReturnsOnlyExactVMFingerprint(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	server := newFakeImageServer()
	server.images[fingerprint] = api.Image{Fingerprint: fingerprint, Type: virtualMachineImageType}
	server.aliases["custom"] = api.ImageAliasesEntry{Name: "custom", Type: virtualMachineImageType, ImageAliasesEntryPut: api.ImageAliasesEntryPut{Target: fingerprint}}
	client := &imageClient{server: server}
	type contextKey string
	ctx := context.WithValue(context.Background(), contextKey("image"), "exact-call")
	resolved, err := client.resolveFingerprint(ctx, "custom")
	if err != nil || resolved != fingerprint {
		t.Fatalf("fingerprint=%q err=%v", resolved, err)
	}
	if server.context != ctx {
		t.Fatal("image lookup did not bind its call context")
	}
	server.images[fingerprint] = api.Image{Fingerprint: fingerprint, Type: "container"}
	if _, err := client.resolveFingerprint(ctx, fingerprint); err == nil || !strings.Contains(err.Error(), "virtual-machine") {
		t.Fatalf("container image resolution error=%v", err)
	}
	if _, err := client.resolveFingerprint(ctx, "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing alias resolution error=%v", err)
	}
}

func TestSDKUnifiedVMImportUpdatesAliasWithETagAndVerifiesPostconditions(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	oldFingerprint := strings.Repeat("b", 64)
	server := newFakeImageServer()
	server.importFingerprint = fingerprint
	server.images[oldFingerprint] = api.Image{Fingerprint: oldFingerprint, Type: virtualMachineImageType}
	server.aliases["dorf-profile-local"] = api.ImageAliasesEntry{
		Name: "dorf-profile-local", Type: virtualMachineImageType,
		ImageAliasesEntryPut: api.ImageAliasesEntryPut{Description: "friendly", Target: oldFingerprint},
	}
	server.aliasETags["dorf-profile-local"] = "alias-etag-before-read"
	client := &imageClient{server: server}
	archive := []byte("one verified unified Incus VM archive")
	ctx := context.Background()
	if err := client.installUnifiedVMArchive(ctx, bytes.NewReader(archive), "dorf.tar.gz", fingerprint, "dorf-profile-local"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(server.importBytes, archive) || server.importPost.Filename != "dorf.tar.gz" ||
		server.importArgs.MetaName != "dorf.tar.gz" || server.importArgs.RootfsFile != nil || server.importArgs.Type != virtualMachineImageType {
		t.Fatalf("unified import post=%#v args=%#v bytes=%q", server.importPost, server.importArgs, server.importBytes)
	}
	if server.updatedAliases != 1 || server.createdAliases != 0 || server.updateETag != "alias-etag-before-read" ||
		server.aliases["dorf-profile-local"].Target != fingerprint || server.aliases["dorf-profile-local"].Description != "friendly" {
		t.Fatalf("alias update count=%d create=%d etag=%q alias=%#v", server.updatedAliases, server.createdAliases, server.updateETag, server.aliases["dorf-profile-local"])
	}
}

func TestSDKUnifiedVMImportCreatesAliasAndRejectsFailedPostcondition(t *testing.T) {
	fingerprint := strings.Repeat("c", 64)
	server := newFakeImageServer()
	server.images[fingerprint] = api.Image{Fingerprint: fingerprint, Type: virtualMachineImageType}
	client := &imageClient{server: server}
	if err := client.installUnifiedVMArchive(context.Background(), bytes.NewReader(nil), "dorf.tar.gz", fingerprint, "dorf-profile-new"); err != nil {
		t.Fatal(err)
	}
	if server.createdAliases != 1 || server.aliases["dorf-profile-new"].Target != fingerprint {
		t.Fatalf("created alias=%#v count=%d", server.aliases["dorf-profile-new"], server.createdAliases)
	}

	changed := server.aliases["dorf-profile-new"]
	changed.Target = strings.Repeat("d", 64)
	server.aliases["dorf-profile-new"] = changed
	server.aliasETags["dorf-profile-new"] = "exact-etag"
	server.ignoreAliasUpdate = true
	err := client.installUnifiedVMArchive(context.Background(), bytes.NewReader(nil), "dorf.tar.gz", fingerprint, "dorf-profile-new")
	if err == nil || !strings.Contains(err.Error(), "verify installed Incus image alias") || server.updateETag != "exact-etag" {
		t.Fatalf("postcondition error=%v update etag=%q", err, server.updateETag)
	}
}
