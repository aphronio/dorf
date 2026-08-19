package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/postgres/dbsql"
	"github.com/aphronio/dorf/internal/spine"
)

var ErrProfileNotFound = errors.New("Dorf Sandbox profile not found")
var profileNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var incusFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type SandboxProfilePatch struct {
	Harness           *string
	IncusArtifact     *string
	IncusNetwork      *string
	IncusDiskSize     *string
	E2BArtifact       *string
	E2BGatewayURL     *string
	E2BSandboxTimeout *time.Duration
	E2BAllowInternet  *bool
}

func (s Store) CreateSandboxProfile(ctx context.Context, profile spine.SandboxProfile) (spine.SandboxProfile, bool, error) {
	profile, err := normalizeSandboxProfile(profile)
	if err != nil {
		return spine.SandboxProfile{}, false, err
	}
	rows, err := dbsql.New(s.DB).InsertSandboxProfile(ctx, insertProfileParams(profile))
	if err != nil {
		return spine.SandboxProfile{}, false, err
	}
	stored, err := s.SandboxProfile(ctx, profile.Name)
	if err != nil {
		return spine.SandboxProfile{}, false, err
	}
	if !sameProfileDefinition(stored, profile) {
		return spine.SandboxProfile{}, false, fmt.Errorf("Sandbox profile %q already exists with a different immutable definition; update it only after all of its Jobs complete cleanup", profile.Name)
	}
	return stored, rows == 1, nil
}

func (s Store) UpdateSandboxProfile(ctx context.Context, name string, patch SandboxProfilePatch) (spine.SandboxProfile, bool, error) {
	name = strings.TrimSpace(name)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.SandboxProfile{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	locked, err := queries.LockSandboxProfile(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return spine.SandboxProfile{}, false, ErrProfileNotFound
	} else if err != nil {
		return spine.SandboxProfile{}, false, err
	}
	current := profileFromLockRow(locked)
	profile, err := applySandboxProfilePatch(current, patch)
	if err != nil {
		return spine.SandboxProfile{}, false, err
	}
	profile, err = normalizeSandboxProfile(profile)
	if err != nil {
		return spine.SandboxProfile{}, false, err
	}
	if sameProfileDefinition(current, profile) {
		row, err := queries.GetSandboxProfile(ctx, name)
		if err != nil {
			return spine.SandboxProfile{}, false, err
		}
		stored := profileFromGetRow(row)
		if err := tx.Commit(); err != nil {
			return spine.SandboxProfile{}, false, err
		}
		return stored, false, nil
	}
	inUse, err := queries.ProfileHasIncompleteJobs(ctx, name)
	if err != nil {
		return spine.SandboxProfile{}, false, err
	}
	if inUse {
		return spine.SandboxProfile{}, false, fmt.Errorf("Sandbox profile %q is immutable while a Job using it has incomplete cleanup", name)
	}
	needsCleanup, err := queries.ProfileVerificationNeedsCleanup(ctx, name)
	if err != nil {
		return spine.SandboxProfile{}, false, err
	}
	if needsCleanup {
		return spine.SandboxProfile{}, false, fmt.Errorf("Sandbox profile %q cannot be updated while its verification Sandbox cleanup is incomplete; rerun dorf profile verify %s", name, name)
	}
	if err := queries.DeleteProfileVerification(ctx, name); err != nil {
		return spine.SandboxProfile{}, false, err
	}
	rows, err := queries.UpdateSandboxProfile(ctx, updateProfileParams(profile))
	if err != nil {
		return spine.SandboxProfile{}, false, err
	}
	if rows != 1 {
		return spine.SandboxProfile{}, false, fmt.Errorf("update Sandbox profile %q affected %d rows", name, rows)
	}
	row, err := queries.GetSandboxProfile(ctx, name)
	if err != nil {
		return spine.SandboxProfile{}, false, err
	}
	stored := profileFromGetRow(row)
	if err := tx.Commit(); err != nil {
		return spine.SandboxProfile{}, false, err
	}
	return stored, true, nil
}

func applySandboxProfilePatch(profile spine.SandboxProfile, patch SandboxProfilePatch) (spine.SandboxProfile, error) {
	if patch.Harness != nil {
		profile.Harness = *patch.Harness
	}
	switch profile.Provider {
	case spine.SandboxProviderIncus:
		if patch.E2BArtifact != nil || patch.E2BGatewayURL != nil || patch.E2BSandboxTimeout != nil || patch.E2BAllowInternet != nil {
			return spine.SandboxProfile{}, fmt.Errorf("Incus profile update does not accept E2B fields")
		}
		if patch.IncusArtifact != nil {
			profile.Artifact = *patch.IncusArtifact
		}
		if patch.IncusNetwork != nil {
			profile.IncusNetwork = *patch.IncusNetwork
		}
		if patch.IncusDiskSize != nil {
			profile.IncusDiskSize = *patch.IncusDiskSize
		}
	case spine.SandboxProviderE2B:
		if patch.IncusArtifact != nil || patch.IncusNetwork != nil || patch.IncusDiskSize != nil {
			return spine.SandboxProfile{}, fmt.Errorf("E2B profile update does not accept Incus fields")
		}
		if patch.E2BArtifact != nil {
			profile.Artifact = *patch.E2BArtifact
		}
		if patch.E2BGatewayURL != nil {
			profile.E2BGatewayURL = *patch.E2BGatewayURL
		}
		if patch.E2BSandboxTimeout != nil {
			profile.E2BSandboxTimeout = *patch.E2BSandboxTimeout
		}
		if patch.E2BAllowInternet != nil {
			profile.E2BAllowInternet = *patch.E2BAllowInternet
		}
	default:
		return spine.SandboxProfile{}, fmt.Errorf("unsupported Sandbox provider %q", profile.Provider)
	}
	return profile, nil
}

func (s Store) SandboxProfile(ctx context.Context, name string) (spine.SandboxProfile, error) {
	row, err := dbsql.New(s.DB).GetSandboxProfile(ctx, strings.TrimSpace(name))
	if errors.Is(err, sql.ErrNoRows) {
		return spine.SandboxProfile{}, ErrProfileNotFound
	}
	if err != nil {
		return spine.SandboxProfile{}, err
	}
	return profileFromGetRow(row), nil
}

func (s Store) SandboxProfiles(ctx context.Context) ([]spine.SandboxProfile, error) {
	rows, err := dbsql.New(s.DB).ListSandboxProfiles(ctx)
	if err != nil {
		return nil, err
	}
	profiles := make([]spine.SandboxProfile, 0, len(rows))
	for _, row := range rows {
		profiles = append(profiles, profileFromListRow(row))
	}
	return profiles, nil
}

func (s Store) DefaultSandboxProfile(ctx context.Context) (spine.SandboxProfile, error) {
	name, err := dbsql.New(s.DB).GetDefaultSandboxProfile(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return spine.SandboxProfile{}, fmt.Errorf("no default Sandbox profile is configured")
	}
	if err != nil {
		return spine.SandboxProfile{}, err
	}
	return s.SandboxProfile(ctx, name)
}

func (s Store) SetDefaultSandboxProfile(ctx context.Context, name string) (spine.SandboxProfile, error) {
	name = strings.TrimSpace(name)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.SandboxProfile{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	if _, err := queries.LockSandboxProfile(ctx, name); errors.Is(err, sql.ErrNoRows) {
		return spine.SandboxProfile{}, ErrProfileNotFound
	} else if err != nil {
		return spine.SandboxProfile{}, err
	}
	row, err := queries.GetSandboxProfile(ctx, name)
	if err != nil {
		return spine.SandboxProfile{}, err
	}
	profile := profileFromGetRow(row)
	if !profile.BaseVerified() {
		return spine.SandboxProfile{}, fmt.Errorf("Sandbox profile %q has not completed Dorf %s verification and cleanup", name, spine.BaseProfileContract)
	}
	if err := queries.ClearDefaultSandboxProfile(ctx); err != nil {
		return spine.SandboxProfile{}, err
	}
	rows, err := queries.SetDefaultSandboxProfile(ctx, name)
	if err != nil {
		return spine.SandboxProfile{}, err
	}
	if rows != 1 {
		return spine.SandboxProfile{}, fmt.Errorf("set default Sandbox profile %q affected %d rows", name, rows)
	}
	if err := tx.Commit(); err != nil {
		return spine.SandboxProfile{}, err
	}
	return s.SandboxProfile(ctx, name)
}

func (s Store) BeginSandboxProfileVerification(ctx context.Context, name string) (spine.SandboxProfile, spine.ProfileVerification, error) {
	nonce, err := reviewNonce()
	if err != nil {
		return spine.SandboxProfile{}, spine.ProfileVerification{}, err
	}
	name = strings.TrimSpace(name)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.SandboxProfile{}, spine.ProfileVerification{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	if _, err := queries.LockSandboxProfile(ctx, name); errors.Is(err, sql.ErrNoRows) {
		return spine.SandboxProfile{}, spine.ProfileVerification{}, ErrProfileNotFound
	} else if err != nil {
		return spine.SandboxProfile{}, spine.ProfileVerification{}, err
	}
	row, err := queries.GetSandboxProfile(ctx, name)
	if err != nil {
		return spine.SandboxProfile{}, spine.ProfileVerification{}, err
	}
	profile := profileFromGetRow(row)
	if profile.Verification != nil && !profile.Verification.ProbeCompletedAt.IsZero() {
		if err := tx.Commit(); err != nil {
			return spine.SandboxProfile{}, spine.ProfileVerification{}, err
		}
		return profile, *profile.Verification, nil
	}
	digest := sha256.Sum256([]byte(name))
	sandboxID := fmt.Sprintf("dorf-profile-%x", digest[:10])
	verificationRow, err := queries.BeginSandboxProfileVerification(ctx, dbsql.BeginSandboxProfileVerificationParams{
		ProfileName: profile.Name, ContractVersion: spine.BaseProfileContract,
		SandboxID: sandboxID, OwnershipNonce: nonce,
	})
	if err != nil {
		return spine.SandboxProfile{}, spine.ProfileVerification{}, err
	}
	if err := tx.Commit(); err != nil {
		return spine.SandboxProfile{}, spine.ProfileVerification{}, err
	}
	verification := verificationFromBeginRow(verificationRow)
	return profile, verification, nil
}

func (s Store) RecordSandboxProfileProbe(ctx context.Context, verification spine.ProfileVerification, harnessVersion string) error {
	harnessVersion = strings.TrimSpace(harnessVersion)
	if harnessVersion == "" {
		return fmt.Errorf("profile verification requires an observed Harness version")
	}
	rows, err := dbsql.New(s.DB).RecordSandboxProfileProbe(ctx, dbsql.RecordSandboxProfileProbeParams{
		HarnessVersion: nullableString(harnessVersion), ProfileName: verification.ProfileName,
		ContractVersion: verification.ContractVersion, SandboxID: verification.SandboxID, OwnershipNonce: verification.OwnershipNonce,
	})
	return expectOneRows(rows, err)
}

func (s Store) RecordSandboxProfileVerificationCleanup(ctx context.Context, verification spine.ProfileVerification) error {
	rows, err := dbsql.New(s.DB).RecordSandboxProfileVerificationCleanup(ctx, dbsql.RecordSandboxProfileVerificationCleanupParams{
		ProfileName: verification.ProfileName, ContractVersion: verification.ContractVersion,
		SandboxID: verification.SandboxID, OwnershipNonce: verification.OwnershipNonce,
	})
	return expectOneRows(rows, err)
}

func (s Store) RecordSandboxProfileVerificationError(ctx context.Context, verification spine.ProfileVerification, failure error) error {
	if failure == nil {
		return nil
	}
	detail := strings.TrimSpace(failure.Error())
	if len(detail) > 2048 {
		detail = detail[:2048]
	}
	rows, err := dbsql.New(s.DB).RecordSandboxProfileVerificationError(ctx, dbsql.RecordSandboxProfileVerificationErrorParams{
		LastError: nullableString(detail), ProfileName: verification.ProfileName,
		ContractVersion: verification.ContractVersion, SandboxID: verification.SandboxID, OwnershipNonce: verification.OwnershipNonce,
	})
	return expectOneRows(rows, err)
}

func normalizeSandboxProfile(profile spine.SandboxProfile) (spine.SandboxProfile, error) {
	profile.Name = strings.TrimSpace(profile.Name)
	profile.Harness = strings.TrimSpace(profile.Harness)
	profile.Artifact = strings.TrimSpace(profile.Artifact)
	profile.IncusNetwork = strings.TrimSpace(profile.IncusNetwork)
	profile.IncusDiskSize = strings.TrimSpace(profile.IncusDiskSize)
	profile.E2BGatewayURL = strings.TrimSpace(profile.E2BGatewayURL)
	profile.Default, profile.CreatedAt, profile.Verification = false, time.Time{}, nil
	if err := ValidateSandboxProfileIdentity(profile.Name, profile.Harness); err != nil {
		return spine.SandboxProfile{}, err
	}
	if profile.Artifact == "" {
		return spine.SandboxProfile{}, fmt.Errorf("Sandbox profile requires an exact provider artifact")
	}
	switch profile.Provider {
	case spine.SandboxProviderIncus:
		if !incusFingerprintPattern.MatchString(profile.Artifact) {
			return spine.SandboxProfile{}, fmt.Errorf("Incus profile artifact must be an exact lowercase 64-hex image fingerprint")
		}
		if profile.IncusNetwork == "" || profile.IncusDiskSize == "" {
			return spine.SandboxProfile{}, fmt.Errorf("Incus profile requires network and disk size")
		}
		profile.E2BGatewayURL, profile.E2BSandboxTimeout, profile.E2BAllowInternet = "", 0, false
	case spine.SandboxProviderE2B:
		if !strings.Contains(profile.Artifact, ":") {
			return spine.SandboxProfile{}, fmt.Errorf("E2B profile artifact must pin an exact template build reference")
		}
		if profile.E2BGatewayURL == "" || profile.E2BSandboxTimeout <= 0 || profile.E2BSandboxTimeout%time.Second != 0 {
			return spine.SandboxProfile{}, fmt.Errorf("E2B profile requires an exact Gateway URL and positive whole-second Sandbox timeout")
		}
		gatewayURL, err := url.Parse(profile.E2BGatewayURL)
		if err != nil || gatewayURL.Scheme != "https" || gatewayURL.Host == "" || gatewayURL.User != nil || gatewayURL.RawQuery != "" || gatewayURL.Fragment != "" || gatewayURL.Path != "/v1" {
			return spine.SandboxProfile{}, fmt.Errorf("E2B profile Gateway URL must be an exact HTTPS /v1 endpoint")
		}
		profile.IncusNetwork, profile.IncusDiskSize = "", ""
	default:
		return spine.SandboxProfile{}, fmt.Errorf("unsupported Sandbox provider %q", profile.Provider)
	}
	return profile, nil
}

// ValidateSandboxProfileIdentity validates the provider-independent fields
// before a command performs an external artifact mutation.
func ValidateSandboxProfileIdentity(name, harness string) error {
	name = strings.TrimSpace(name)
	harness = strings.TrimSpace(harness)
	if !profileNamePattern.MatchString(name) {
		return fmt.Errorf("Sandbox profile name must match %s", profileNamePattern)
	}
	if harness != "codex" && harness != "pi" {
		return fmt.Errorf("Sandbox profile Harness must be codex or pi")
	}
	return nil
}

func insertProfileParams(profile spine.SandboxProfile) dbsql.InsertSandboxProfileParams {
	return dbsql.InsertSandboxProfileParams{
		Name: profile.Name, Provider: string(profile.Provider), Harness: profile.Harness, Artifact: profile.Artifact,
		IncusNetwork: nullableString(profile.IncusNetwork), IncusDiskSize: nullableString(profile.IncusDiskSize),
		E2bGatewayURL: nullableString(profile.E2BGatewayURL), E2bSandboxTimeoutSeconds: nullableInt64(int64(profile.E2BSandboxTimeout / time.Second)),
		E2bAllowInternet: nullableBool(profile.Provider == spine.SandboxProviderE2B, profile.E2BAllowInternet),
	}
}

func updateProfileParams(profile spine.SandboxProfile) dbsql.UpdateSandboxProfileParams {
	insert := insertProfileParams(profile)
	return dbsql.UpdateSandboxProfileParams{
		Provider: insert.Provider, Harness: insert.Harness, Artifact: insert.Artifact,
		IncusNetwork: insert.IncusNetwork, IncusDiskSize: insert.IncusDiskSize,
		E2bGatewayURL: insert.E2bGatewayURL, E2bSandboxTimeoutSeconds: insert.E2bSandboxTimeoutSeconds,
		E2bAllowInternet: insert.E2bAllowInternet, Name: profile.Name,
	}
}

func nullableInt64(value int64) sql.NullInt64 {
	if value == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: value, Valid: true}
}

func nullableBool(valid, value bool) sql.NullBool { return sql.NullBool{Bool: value, Valid: valid} }

func sameProfileDefinition(left, right spine.SandboxProfile) bool {
	left.Default, right.Default = false, false
	left.CreatedAt, right.CreatedAt = time.Time{}, time.Time{}
	left.Verification, right.Verification = nil, nil
	return left == right
}

func profileFromGetRow(row dbsql.GetSandboxProfileRow) spine.SandboxProfile {
	return profileFromColumns(row.Name, row.Provider, row.Harness, row.Artifact, row.IncusNetwork, row.IncusDiskSize,
		row.E2bGatewayURL, row.E2bSandboxTimeoutSeconds, row.E2bAllowInternet, row.IsDefault, row.CreatedAt,
		row.VerificationContract, row.VerificationSandboxID, row.VerificationOwnershipNonce, row.VerificationHarnessVersion,
		row.AttemptedAt, row.ProbeCompletedAt, row.CleanedAt, row.VerificationLastError)
}

func profileFromListRow(row dbsql.ListSandboxProfilesRow) spine.SandboxProfile {
	return profileFromColumns(row.Name, row.Provider, row.Harness, row.Artifact, row.IncusNetwork, row.IncusDiskSize,
		row.E2bGatewayURL, row.E2bSandboxTimeoutSeconds, row.E2bAllowInternet, row.IsDefault, row.CreatedAt,
		row.VerificationContract, row.VerificationSandboxID, row.VerificationOwnershipNonce, row.VerificationHarnessVersion,
		row.AttemptedAt, row.ProbeCompletedAt, row.CleanedAt, row.VerificationLastError)
}

func profileFromLockRow(row dbsql.LockSandboxProfileRow) spine.SandboxProfile {
	return spine.SandboxProfile{
		Name: row.Name, Provider: spine.SandboxProvider(row.Provider), Harness: row.Harness, Artifact: row.Artifact,
		IncusNetwork: row.IncusNetwork, IncusDiskSize: row.IncusDiskSize,
		E2BGatewayURL: row.E2bGatewayURL, E2BSandboxTimeout: time.Duration(row.E2bSandboxTimeoutSeconds) * time.Second,
		E2BAllowInternet: row.E2bAllowInternet, Default: row.IsDefault, CreatedAt: row.CreatedAt,
	}
}

func profileFromColumns(name, provider, harness, artifact, network, disk, gatewayURL string, timeoutSeconds int64, allowInternet, isDefault bool, createdAt time.Time,
	contract, sandboxID, nonce, harnessVersion string, attemptedAt, probeAt, cleanedAt sql.NullTime, lastError string) spine.SandboxProfile {
	profile := spine.SandboxProfile{
		Name: name, Provider: spine.SandboxProvider(provider), Harness: harness, Artifact: artifact,
		IncusNetwork: network, IncusDiskSize: disk, E2BGatewayURL: gatewayURL,
		E2BSandboxTimeout: time.Duration(timeoutSeconds) * time.Second, E2BAllowInternet: allowInternet,
		Default: isDefault, CreatedAt: createdAt,
	}
	if contract != "" {
		profile.Verification = &spine.ProfileVerification{
			ProfileName: name, ContractVersion: contract, SandboxID: sandboxID, OwnershipNonce: nonce,
			HarnessVersion: harnessVersion, AttemptedAt: timeValue(attemptedAt), ProbeCompletedAt: timeValue(probeAt),
			CleanedAt: timeValue(cleanedAt), LastError: lastError,
		}
	}
	return profile
}

func verificationFromBeginRow(row dbsql.BeginSandboxProfileVerificationRow) spine.ProfileVerification {
	return spine.ProfileVerification{
		ProfileName: row.ProfileName, ContractVersion: row.ContractVersion, SandboxID: row.SandboxID,
		OwnershipNonce: row.OwnershipNonce, HarnessVersion: row.HarnessVersion, AttemptedAt: row.AttemptedAt,
		ProbeCompletedAt: timeValue(row.ProbeCompletedAt), CleanedAt: timeValue(row.CleanedAt), LastError: row.LastError,
	}
}
