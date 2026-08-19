package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/repository"
	"github.com/aphronio/dorf/internal/spine"
)

func prepareInvestigationSource(ctx context.Context, blobs blob.Store, remoteRepository, localRepository, revision string) (spine.CodebaseInvestigationSource, bool, error) {
	remoteRepository = strings.TrimSpace(remoteRepository)
	localRepository = strings.TrimSpace(localRepository)
	if (remoteRepository == "") == (localRepository == "") {
		return spine.CodebaseInvestigationSource{}, false, fmt.Errorf("codebase-investigation requires exactly one of --repo or --local-repo")
	}
	if remoteRepository != "" {
		return spine.CodebaseInvestigationSource{Kind: spine.InvestigationSourceRemote, Repository: remoteRepository, Revision: strings.TrimSpace(revision)}, false, nil
	}
	bundle, err := repository.BundleLocalRevision(ctx, localRepository, revision)
	if err != nil {
		return spine.CodebaseInvestigationSource{}, false, err
	}
	retained, err := blobs.Put(bundle.Contents)
	if err != nil {
		return spine.CodebaseInvestigationSource{}, false, fmt.Errorf("retain local repository bundle before admission: %w", err)
	}
	return spine.CodebaseInvestigationSource{
		Kind: spine.InvestigationSourceGitBundle, Revision: bundle.Revision,
		BundleDigest: retained.Digest, BundleByteSize: retained.ByteSize,
	}, bundle.WorkingTreeChangesExcluded, nil
}
