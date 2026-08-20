package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/repository"
)

func prepareInvestigationSource(ctx context.Context, blobs blob.Store, remoteRepository, localRepository, revision string) (investigation.Source, bool, error) {
	remoteRepository = strings.TrimSpace(remoteRepository)
	localRepository = strings.TrimSpace(localRepository)
	if (remoteRepository == "") == (localRepository == "") {
		return investigation.Source{}, false, fmt.Errorf("codebase-investigation requires exactly one of --repo or --local-repo")
	}
	if remoteRepository != "" {
		return investigation.Source{Kind: investigation.SourceRemote, Repository: remoteRepository, Revision: strings.TrimSpace(revision)}, false, nil
	}
	bundle, err := repository.BundleLocalRevision(ctx, localRepository, revision)
	if err != nil {
		return investigation.Source{}, false, err
	}
	retained, err := blobs.Put(bundle.Contents)
	if err != nil {
		return investigation.Source{}, false, fmt.Errorf("retain local repository bundle before admission: %w", err)
	}
	return investigation.Source{
		Kind: investigation.SourceGitBundle, Revision: bundle.Revision,
		BundleDigest: retained.Digest, BundleByteSize: retained.ByteSize,
	}, bundle.WorkingTreeChangesExcluded, nil
}
