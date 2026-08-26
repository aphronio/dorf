package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

var ErrProfileNotFound = errors.New("Dorf Sandbox profile not found")
var profileNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var incusFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var incusIdentifierPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var incusDiskSizePattern = regexp.MustCompile(`^[1-9][0-9]*(MiB|GiB|TiB)$`)

type SandboxProfilePatch struct {
	Harness                    *string
	IncusArtifact              *string
	IncusEndpointAuthorityHash *string
	IncusProject               *string
	IncusStoragePool           *string
	IncusNetwork               *string
	IncusDiskSize              *string
	IncusGatewayURL            *string
	E2BArtifact                *string
	E2BGatewayURL              *string
	E2BSandboxTimeout          *time.Duration
	E2BAllowInternet           *bool
}

func (s Store) WithSandboxProfileVerification(ctx context.Context, name string, work func(context.Context) error) (resultErr error) {
	name = strings.TrimSpace(name)
	if name == "" || work == nil {
		return fmt.Errorf("Sandbox profile verification requires a profile name and operation")
	}
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, fmt.Errorf("release Sandbox profile verification ownership: %w", err))
		}
	}()
	lockKey := "dorf:sandbox-profile-verification:" + name
	var acquired bool
	if err := tx.QueryRowContext(ctx, `select pg_try_advisory_xact_lock(hashtextextended($1,0))`, lockKey).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf("Sandbox profile %q verification is already running", name)
	}
	return work(ctx)
}

func (s Store) CreateSandboxProfile(ctx context.Context, profile core.SandboxProfile) (core.SandboxProfile, bool, error) {
	profile, err := normalizeSandboxProfile(profile)
	if err != nil {
		return core.SandboxProfile{}, false, err
	}
	rows, err := dbsql.New(s.DB).InsertSandboxProfile(ctx, insertProfileParams(profile))
	if err != nil {
		return core.SandboxProfile{}, false, err
	}
	stored, err := s.SandboxProfile(ctx, profile.Name)
	if err != nil {
		return core.SandboxProfile{}, false, err
	}
	if !sameProfileDefinition(stored, profile) {
		return core.SandboxProfile{}, false, fmt.Errorf("Sandbox profile %q already exists with a different immutable definition; update it only after all of its Jobs complete cleanup", profile.Name)
	}
	return stored, rows == 1, nil
}

func (s Store) UpdateSandboxProfile(ctx context.Context, name string, patch SandboxProfilePatch) (core.SandboxProfile, bool, error) {
	name = strings.TrimSpace(name)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.SandboxProfile{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	locked, err := queries.LockSandboxProfile(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return core.SandboxProfile{}, false, ErrProfileNotFound
	} else if err != nil {
		return core.SandboxProfile{}, false, err
	}
	current := profileFromLockRow(locked)
	profile, err := applySandboxProfilePatch(current, patch)
	if err != nil {
		return core.SandboxProfile{}, false, err
	}
	profile, err = normalizeSandboxProfile(profile)
	if err != nil {
		return core.SandboxProfile{}, false, err
	}
	if sameProfileDefinition(current, profile) {
		row, err := queries.GetSandboxProfile(ctx, name)
		if err != nil {
			return core.SandboxProfile{}, false, err
		}
		stored := profileFromGetRow(row)
		if err := tx.Commit(); err != nil {
			return core.SandboxProfile{}, false, err
		}
		return stored, false, nil
	}
	inUse, err := queries.ProfileHasIncompleteJobs(ctx, name)
	if err != nil {
		return core.SandboxProfile{}, false, err
	}
	if inUse {
		return core.SandboxProfile{}, false, fmt.Errorf("Sandbox profile %q is immutable while a Job using it has incomplete cleanup", name)
	}
	needsCleanup, err := queries.ProfileVerificationNeedsCleanup(ctx, name)
	if err != nil {
		return core.SandboxProfile{}, false, err
	}
	if needsCleanup {
		return core.SandboxProfile{}, false, fmt.Errorf("Sandbox profile %q cannot be updated while its verification Sandbox cleanup is incomplete; rerun dorf profile verify %s", name, name)
	}
	if err := queries.DeleteProfileVerification(ctx, name); err != nil {
		return core.SandboxProfile{}, false, err
	}
	rows, err := queries.UpdateSandboxProfile(ctx, updateProfileParams(profile))
	if err != nil {
		return core.SandboxProfile{}, false, err
	}
	if rows != 1 {
		return core.SandboxProfile{}, false, fmt.Errorf("update Sandbox profile %q affected %d rows", name, rows)
	}
	row, err := queries.GetSandboxProfile(ctx, name)
	if err != nil {
		return core.SandboxProfile{}, false, err
	}
	stored := profileFromGetRow(row)
	if err := tx.Commit(); err != nil {
		return core.SandboxProfile{}, false, err
	}
	return stored, true, nil
}

func applySandboxProfilePatch(profile core.SandboxProfile, patch SandboxProfilePatch) (core.SandboxProfile, error) {
	if patch.Harness != nil {
		profile.Harness = *patch.Harness
	}
	switch profile.Provider {
	case core.SandboxProviderIncus:
		if patch.E2BArtifact != nil || patch.E2BGatewayURL != nil || patch.E2BSandboxTimeout != nil || patch.E2BAllowInternet != nil {
			return core.SandboxProfile{}, fmt.Errorf("Incus profile update does not accept E2B fields")
		}
		if patch.IncusArtifact != nil {
			profile.Artifact = *patch.IncusArtifact
		}
		if patch.IncusEndpointAuthorityHash != nil {
			profile.IncusEndpointAuthorityHash = *patch.IncusEndpointAuthorityHash
		}
		if patch.IncusProject != nil {
			profile.IncusProject = *patch.IncusProject
		}
		if patch.IncusStoragePool != nil {
			profile.IncusStoragePool = *patch.IncusStoragePool
		}
		if patch.IncusNetwork != nil {
			profile.IncusNetwork = *patch.IncusNetwork
		}
		if patch.IncusDiskSize != nil {
			profile.IncusDiskSize = *patch.IncusDiskSize
		}
		if patch.IncusGatewayURL != nil {
			profile.IncusGatewayURL = *patch.IncusGatewayURL
		}
	case core.SandboxProviderE2B:
		if patch.IncusArtifact != nil || patch.IncusEndpointAuthorityHash != nil || patch.IncusProject != nil ||
			patch.IncusStoragePool != nil || patch.IncusNetwork != nil || patch.IncusDiskSize != nil || patch.IncusGatewayURL != nil {
			return core.SandboxProfile{}, fmt.Errorf("E2B profile update does not accept Incus fields")
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
		return core.SandboxProfile{}, fmt.Errorf("unsupported Sandbox provider %q", profile.Provider)
	}
	return profile, nil
}

func (s Store) SandboxProfile(ctx context.Context, name string) (core.SandboxProfile, error) {
	row, err := dbsql.New(s.DB).GetSandboxProfile(ctx, strings.TrimSpace(name))
	if errors.Is(err, sql.ErrNoRows) {
		return core.SandboxProfile{}, ErrProfileNotFound
	}
	if err != nil {
		return core.SandboxProfile{}, err
	}
	return profileFromGetRow(row), nil
}

func (s Store) SandboxProfiles(ctx context.Context) ([]core.SandboxProfile, error) {
	rows, err := dbsql.New(s.DB).ListSandboxProfiles(ctx)
	if err != nil {
		return nil, err
	}
	profiles := make([]core.SandboxProfile, 0, len(rows))
	for _, row := range rows {
		profiles = append(profiles, profileFromListRow(row))
	}
	return profiles, nil
}

func (s Store) DefaultSandboxProfile(ctx context.Context) (core.SandboxProfile, error) {
	name, err := dbsql.New(s.DB).GetDefaultSandboxProfile(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return core.SandboxProfile{}, fmt.Errorf("no default Sandbox profile is configured")
	}
	if err != nil {
		return core.SandboxProfile{}, err
	}
	return s.SandboxProfile(ctx, name)
}

func (s Store) SetDefaultSandboxProfile(ctx context.Context, name string) (core.SandboxProfile, error) {
	name = strings.TrimSpace(name)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.SandboxProfile{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	if _, err := queries.LockSandboxProfile(ctx, name); errors.Is(err, sql.ErrNoRows) {
		return core.SandboxProfile{}, ErrProfileNotFound
	} else if err != nil {
		return core.SandboxProfile{}, err
	}
	row, err := queries.GetSandboxProfile(ctx, name)
	if err != nil {
		return core.SandboxProfile{}, err
	}
	profile := profileFromGetRow(row)
	if profile.DefinitionHash != profile.CurrentDefinitionHash() {
		return core.SandboxProfile{}, fmt.Errorf("Sandbox profile %q definition does not match its retained hash; update it explicitly before verification", name)
	}
	if !profile.BaseVerified() {
		return core.SandboxProfile{}, fmt.Errorf("Sandbox profile %q has not completed Dorf %s verification and cleanup", name, core.BaseProfileContract)
	}
	if err := queries.ClearDefaultSandboxProfile(ctx); err != nil {
		return core.SandboxProfile{}, err
	}
	rows, err := queries.SetDefaultSandboxProfile(ctx, name)
	if err != nil {
		return core.SandboxProfile{}, err
	}
	if rows != 1 {
		return core.SandboxProfile{}, fmt.Errorf("set default Sandbox profile %q affected %d rows", name, rows)
	}
	if err := tx.Commit(); err != nil {
		return core.SandboxProfile{}, err
	}
	return s.SandboxProfile(ctx, name)
}

func (s Store) BeginSandboxProfileVerification(ctx context.Context, name string) (core.SandboxProfile, core.ProfileVerification, error) {
	nonce, err := reviewNonce()
	if err != nil {
		return core.SandboxProfile{}, core.ProfileVerification{}, err
	}
	name = strings.TrimSpace(name)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.SandboxProfile{}, core.ProfileVerification{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	if _, err := queries.LockSandboxProfile(ctx, name); errors.Is(err, sql.ErrNoRows) {
		return core.SandboxProfile{}, core.ProfileVerification{}, ErrProfileNotFound
	} else if err != nil {
		return core.SandboxProfile{}, core.ProfileVerification{}, err
	}
	row, err := queries.GetSandboxProfile(ctx, name)
	if err != nil {
		return core.SandboxProfile{}, core.ProfileVerification{}, err
	}
	profile := profileFromGetRow(row)
	if profile.DefinitionHash != profile.CurrentDefinitionHash() {
		return core.SandboxProfile{}, core.ProfileVerification{}, fmt.Errorf("Sandbox profile %q definition does not match its retained hash; update it explicitly before verification", name)
	}
	if profile.Verification != nil && !profile.Verification.ProbeCompletedAt.IsZero() && profile.Verification.CleanedAt.IsZero() {
		if err := tx.Commit(); err != nil {
			return core.SandboxProfile{}, core.ProfileVerification{}, err
		}
		return profile, *profile.Verification, nil
	}
	if profile.Verification != nil && !profile.Verification.ProbeCompletedAt.IsZero() {
		if err := queries.DeleteProfileVerification(ctx, name); err != nil {
			return core.SandboxProfile{}, core.ProfileVerification{}, err
		}
		profile.Verification = nil
	}
	digest := sha256.Sum256([]byte(name))
	sandboxID := fmt.Sprintf("dorf-profile-%x", digest[:10])
	verificationRow, err := queries.BeginSandboxProfileVerification(ctx, dbsql.BeginSandboxProfileVerificationParams{
		ProfileName: profile.Name, ContractVersion: core.BaseProfileContract, DefinitionHash: profile.DefinitionHash,
		SandboxID: sandboxID, OwnershipNonce: nonce,
	})
	if err != nil {
		return core.SandboxProfile{}, core.ProfileVerification{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.SandboxProfile{}, core.ProfileVerification{}, err
	}
	verification := verificationFromBeginRow(verificationRow)
	return profile, verification, nil
}

func (s Store) RecordSandboxProfileProbe(ctx context.Context, verification core.ProfileVerification, harnessVersion string) error {
	harnessVersion = strings.TrimSpace(harnessVersion)
	if harnessVersion == "" {
		return fmt.Errorf("profile verification requires an observed Harness version")
	}
	rows, err := dbsql.New(s.DB).RecordSandboxProfileProbe(ctx, dbsql.RecordSandboxProfileProbeParams{
		HarnessVersion: nullableString(harnessVersion), ProfileName: verification.ProfileName,
		ContractVersion: verification.ContractVersion, DefinitionHash: verification.DefinitionHash,
		SandboxID: verification.SandboxID, OwnershipNonce: verification.OwnershipNonce,
	})
	return expectOneRows(rows, err)
}

func (s Store) RecordSandboxProfileVerificationCleanup(ctx context.Context, verification core.ProfileVerification) error {
	rows, err := dbsql.New(s.DB).RecordSandboxProfileVerificationCleanup(ctx, dbsql.RecordSandboxProfileVerificationCleanupParams{
		ProfileName: verification.ProfileName, ContractVersion: verification.ContractVersion, DefinitionHash: verification.DefinitionHash,
		SandboxID: verification.SandboxID, OwnershipNonce: verification.OwnershipNonce,
	})
	return expectOneRows(rows, err)
}

func (s Store) RecordSandboxProfileVerificationError(ctx context.Context, verification core.ProfileVerification, failure error) error {
	if failure == nil {
		return nil
	}
	detail := strings.TrimSpace(failure.Error())
	if len(detail) > 2048 {
		detail = detail[:2048]
	}
	rows, err := dbsql.New(s.DB).RecordSandboxProfileVerificationError(ctx, dbsql.RecordSandboxProfileVerificationErrorParams{
		LastError: nullableString(detail), ProfileName: verification.ProfileName,
		ContractVersion: verification.ContractVersion, DefinitionHash: verification.DefinitionHash,
		SandboxID: verification.SandboxID, OwnershipNonce: verification.OwnershipNonce,
	})
	return expectOneRows(rows, err)
}

// RecordSandboxProfileUnavailable atomically fences new admission through an
// exact verified profile and leaves the affected Job at its current fact.
// Existing resources remain recoverable by cleanup through the pinned profile.
func (s Store) RecordSandboxProfileUnavailable(ctx context.Context, jobID, profileName, source string, failure error) error {
	jobID, profileName, source = strings.TrimSpace(jobID), strings.TrimSpace(profileName), strings.TrimSpace(source)
	if jobID == "" || profileName == "" || source == "" || failure == nil {
		return fmt.Errorf("unavailable Sandbox profile requires Job ID, profile, exact source, and failure")
	}
	detail := strings.TrimSpace(failure.Error())
	if detail == "" {
		return fmt.Errorf("unavailable Sandbox profile requires failure detail")
	}
	if len(detail) > 2048 {
		detail = detail[:2048]
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	if _, err := queries.LockSandboxProfile(ctx, profileName); errors.Is(err, sql.ErrNoRows) {
		return ErrProfileNotFound
	} else if err != nil {
		return err
	}
	jobProfile, err := queries.GetJobSandboxProfileForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	if jobProfile != profileName {
		return fmt.Errorf("Job %s pins Sandbox profile %q, not %q", jobID, jobProfile, profileName)
	}
	rows, err := queries.MarkSandboxProfileUnavailable(ctx, dbsql.MarkSandboxProfileUnavailableParams{
		LastError: nullableString(detail), ProfileName: profileName, ContractVersion: core.BaseProfileContract,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("Sandbox profile %q has no settled Dorf %s verification to invalidate", profileName, core.BaseProfileContract)
	}
	rows, err = queries.SetWorkflowAttention(ctx, dbsql.SetWorkflowAttentionParams{
		JobID: jobID, Source: nullableString(source), Detail: nullableString(detail),
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("Job %s already has attention owned by a different fact", jobID)
	}
	return tx.Commit()
}

func normalizeSandboxProfile(profile core.SandboxProfile) (core.SandboxProfile, error) {
	profile.Name = strings.TrimSpace(profile.Name)
	profile.Harness = strings.TrimSpace(profile.Harness)
	profile.Artifact = strings.TrimSpace(profile.Artifact)
	profile.IncusEndpointAuthorityHash = strings.TrimSpace(profile.IncusEndpointAuthorityHash)
	profile.IncusProject = strings.TrimSpace(profile.IncusProject)
	profile.IncusStoragePool = strings.TrimSpace(profile.IncusStoragePool)
	profile.IncusNetwork = strings.TrimSpace(profile.IncusNetwork)
	profile.IncusDiskSize = strings.TrimSpace(profile.IncusDiskSize)
	profile.IncusGatewayURL = strings.TrimSpace(profile.IncusGatewayURL)
	profile.E2BGatewayURL = strings.TrimSpace(profile.E2BGatewayURL)
	profile.DefinitionHash, profile.Default, profile.CreatedAt, profile.Verification = "", false, time.Time{}, nil
	if err := ValidateSandboxProfileIdentity(profile.Name, profile.Harness); err != nil {
		return core.SandboxProfile{}, err
	}
	if profile.Artifact == "" {
		return core.SandboxProfile{}, fmt.Errorf("Sandbox profile requires an exact provider artifact")
	}
	switch profile.Provider {
	case core.SandboxProviderIncus:
		if !incusFingerprintPattern.MatchString(profile.Artifact) {
			return core.SandboxProfile{}, fmt.Errorf("Incus profile artifact must be an exact lowercase 64-hex image fingerprint")
		}
		if err := ValidateIncusProfileSettings(profile.IncusEndpointAuthorityHash, profile.IncusProject, profile.IncusStoragePool,
			profile.IncusNetwork, profile.IncusDiskSize, profile.IncusGatewayURL); err != nil {
			return core.SandboxProfile{}, err
		}
		profile.E2BGatewayURL, profile.E2BSandboxTimeout, profile.E2BAllowInternet = "", 0, false
	case core.SandboxProviderE2B:
		if !strings.Contains(profile.Artifact, ":") {
			return core.SandboxProfile{}, fmt.Errorf("E2B profile artifact must pin an exact template build reference")
		}
		if profile.E2BGatewayURL == "" || profile.E2BSandboxTimeout <= 0 || profile.E2BSandboxTimeout%time.Second != 0 {
			return core.SandboxProfile{}, fmt.Errorf("E2B profile requires an exact Gateway URL and positive whole-second Sandbox timeout")
		}
		gatewayURL, err := url.Parse(profile.E2BGatewayURL)
		if err != nil || gatewayURL.Scheme != "https" || gatewayURL.Host == "" || gatewayURL.User != nil || gatewayURL.RawQuery != "" || gatewayURL.Fragment != "" || gatewayURL.Path != "/v1" {
			return core.SandboxProfile{}, fmt.Errorf("E2B profile Gateway URL must be an exact HTTPS /v1 endpoint")
		}
		profile.IncusEndpointAuthorityHash, profile.IncusProject, profile.IncusStoragePool = "", "", ""
		profile.IncusNetwork, profile.IncusDiskSize, profile.IncusGatewayURL = "", "", ""
	default:
		return core.SandboxProfile{}, fmt.Errorf("unsupported Sandbox provider %q", profile.Provider)
	}
	profile.DefinitionHash = profile.CurrentDefinitionHash()
	return profile, nil
}

// ValidateIncusProfileSettings validates the complete provider definition
// before profile tooling performs an external image lookup or import.
func ValidateIncusProfileSettings(authorityHash, project, storagePool, network, diskSize, gatewayURL string) error {
	authorityHash, project, storagePool = strings.TrimSpace(authorityHash), strings.TrimSpace(project), strings.TrimSpace(storagePool)
	network, diskSize, gatewayURL = strings.TrimSpace(network), strings.TrimSpace(diskSize), strings.TrimSpace(gatewayURL)
	if !incusFingerprintPattern.MatchString(authorityHash) {
		return fmt.Errorf("Incus profile requires the exact Deployment endpoint authority hash")
	}
	for _, field := range []struct{ label, value string }{
		{label: "project", value: project},
		{label: "storage pool", value: storagePool},
		{label: "network", value: network},
	} {
		if !incusIdentifierPattern.MatchString(field.value) {
			return fmt.Errorf("Incus profile %s must match %s", field.label, incusIdentifierPattern)
		}
	}
	if !incusDiskSizePattern.MatchString(diskSize) {
		return fmt.Errorf("Incus profile disk size must use canonical MiB, GiB, or TiB units")
	}
	return validateIncusGatewayURL(gatewayURL)
}

func validateIncusGatewayURL(value string) error {
	gateway, err := url.Parse(value)
	if err != nil || gateway.Host == "" || gateway.User != nil || gateway.Path != "/v1" || gateway.RawPath != "" ||
		gateway.RawQuery != "" || gateway.ForceQuery || gateway.Fragment != "" || gateway.Opaque != "" {
		return fmt.Errorf("Incus profile Gateway URL must be exact HTTPS /v1 or HTTP /v1 on a private non-loopback IP")
	}
	switch gateway.Scheme {
	case "https":
		return nil
	case "http":
		ip := net.ParseIP(gateway.Hostname())
		if ip == nil || !privateGatewayIP(ip) || ip.IsLoopback() {
			return fmt.Errorf("Incus profile HTTP Gateway URL must use a private non-loopback IP and exact /v1 path")
		}
		return nil
	default:
		return fmt.Errorf("Incus profile Gateway URL must be exact HTTPS /v1 or HTTP /v1 on a private non-loopback IP")
	}
}

func privateGatewayIP(ip net.IP) bool {
	ipv4 := ip.To4()
	if ipv4 == nil {
		return false
	}
	if ipv4.IsPrivate() {
		return true
	}
	return ipv4[0] == 100 && ipv4[1]&0xc0 == 64 // RFC 6598, including common VPN addressing.
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

func insertProfileParams(profile core.SandboxProfile) dbsql.InsertSandboxProfileParams {
	return dbsql.InsertSandboxProfileParams{
		Name: profile.Name, Provider: string(profile.Provider), Harness: profile.Harness, Artifact: profile.Artifact,
		DefinitionHash: profile.DefinitionHash, IncusEndpointAuthorityHash: nullableString(profile.IncusEndpointAuthorityHash),
		IncusProject: nullableString(profile.IncusProject), IncusStoragePool: nullableString(profile.IncusStoragePool),
		IncusNetwork: nullableString(profile.IncusNetwork), IncusDiskSize: nullableString(profile.IncusDiskSize),
		IncusGatewayURL: nullableString(profile.IncusGatewayURL),
		E2bGatewayURL:   nullableString(profile.E2BGatewayURL), E2bSandboxTimeoutSeconds: nullableInt64(int64(profile.E2BSandboxTimeout / time.Second)),
		E2bAllowInternet: nullableBool(profile.Provider == core.SandboxProviderE2B, profile.E2BAllowInternet),
	}
}

func updateProfileParams(profile core.SandboxProfile) dbsql.UpdateSandboxProfileParams {
	insert := insertProfileParams(profile)
	return dbsql.UpdateSandboxProfileParams{
		Provider: insert.Provider, Harness: insert.Harness, Artifact: insert.Artifact,
		DefinitionHash: insert.DefinitionHash, IncusEndpointAuthorityHash: insert.IncusEndpointAuthorityHash,
		IncusProject: insert.IncusProject, IncusStoragePool: insert.IncusStoragePool,
		IncusNetwork: insert.IncusNetwork, IncusDiskSize: insert.IncusDiskSize,
		IncusGatewayURL: insert.IncusGatewayURL,
		E2bGatewayURL:   insert.E2bGatewayURL, E2bSandboxTimeoutSeconds: insert.E2bSandboxTimeoutSeconds,
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

func sameProfileDefinition(left, right core.SandboxProfile) bool {
	left.Default, right.Default = false, false
	left.CreatedAt, right.CreatedAt = time.Time{}, time.Time{}
	left.Verification, right.Verification = nil, nil
	return left == right
}

func profileFromGetRow(row dbsql.GetSandboxProfileRow) core.SandboxProfile {
	return profileFromColumns(row.Name, row.Provider, row.Harness, row.Artifact, row.DefinitionHash,
		row.IncusEndpointAuthorityHash, row.IncusProject, row.IncusStoragePool, row.IncusNetwork, row.IncusDiskSize, row.IncusGatewayURL,
		row.E2bGatewayURL, row.E2bSandboxTimeoutSeconds, row.E2bAllowInternet, row.IsDefault, row.CreatedAt,
		row.VerificationContract, row.VerificationDefinitionHash, row.VerificationSandboxID, row.VerificationOwnershipNonce, row.VerificationHarnessVersion,
		row.AttemptedAt, row.ProbeCompletedAt, row.CleanedAt, row.VerificationLastError)
}

func profileFromListRow(row dbsql.ListSandboxProfilesRow) core.SandboxProfile {
	return profileFromColumns(row.Name, row.Provider, row.Harness, row.Artifact, row.DefinitionHash,
		row.IncusEndpointAuthorityHash, row.IncusProject, row.IncusStoragePool, row.IncusNetwork, row.IncusDiskSize, row.IncusGatewayURL,
		row.E2bGatewayURL, row.E2bSandboxTimeoutSeconds, row.E2bAllowInternet, row.IsDefault, row.CreatedAt,
		row.VerificationContract, row.VerificationDefinitionHash, row.VerificationSandboxID, row.VerificationOwnershipNonce, row.VerificationHarnessVersion,
		row.AttemptedAt, row.ProbeCompletedAt, row.CleanedAt, row.VerificationLastError)
}

func profileFromLockRow(row dbsql.LockSandboxProfileRow) core.SandboxProfile {
	return core.SandboxProfile{
		Name: row.Name, Provider: core.SandboxProvider(row.Provider), Harness: row.Harness, Artifact: row.Artifact,
		DefinitionHash: row.DefinitionHash, IncusEndpointAuthorityHash: row.IncusEndpointAuthorityHash,
		IncusProject: row.IncusProject, IncusStoragePool: row.IncusStoragePool,
		IncusNetwork: row.IncusNetwork, IncusDiskSize: row.IncusDiskSize,
		IncusGatewayURL: row.IncusGatewayURL,
		E2BGatewayURL:   row.E2bGatewayURL, E2BSandboxTimeout: time.Duration(row.E2bSandboxTimeoutSeconds) * time.Second,
		E2BAllowInternet: row.E2bAllowInternet, Default: row.IsDefault, CreatedAt: row.CreatedAt,
	}
}

func profileFromColumns(name, provider, harness, artifact, definitionHash, incusAuthorityHash, incusProject, incusStoragePool, network, disk, incusGatewayURL, e2bGatewayURL string,
	timeoutSeconds int64, allowInternet, isDefault bool, createdAt time.Time,
	contract, verificationDefinitionHash, sandboxID, nonce, harnessVersion string, attemptedAt, probeAt, cleanedAt sql.NullTime, lastError string) core.SandboxProfile {
	profile := core.SandboxProfile{
		Name: name, Provider: core.SandboxProvider(provider), Harness: harness, Artifact: artifact,
		DefinitionHash: definitionHash, IncusEndpointAuthorityHash: incusAuthorityHash,
		IncusProject: incusProject, IncusStoragePool: incusStoragePool,
		IncusNetwork: network, IncusDiskSize: disk, IncusGatewayURL: incusGatewayURL, E2BGatewayURL: e2bGatewayURL,
		E2BSandboxTimeout: time.Duration(timeoutSeconds) * time.Second, E2BAllowInternet: allowInternet,
		Default: isDefault, CreatedAt: createdAt,
	}
	if contract != "" {
		profile.Verification = &core.ProfileVerification{
			ProfileName: name, ContractVersion: contract, DefinitionHash: verificationDefinitionHash,
			SandboxID: sandboxID, OwnershipNonce: nonce,
			HarnessVersion: harnessVersion, AttemptedAt: timeValue(attemptedAt), ProbeCompletedAt: timeValue(probeAt),
			CleanedAt: timeValue(cleanedAt), LastError: lastError,
		}
	}
	return profile
}

func verificationFromBeginRow(row dbsql.BeginSandboxProfileVerificationRow) core.ProfileVerification {
	return core.ProfileVerification{
		ProfileName: row.ProfileName, ContractVersion: row.ContractVersion, DefinitionHash: row.DefinitionHash, SandboxID: row.SandboxID,
		OwnershipNonce: row.OwnershipNonce, HarnessVersion: row.HarnessVersion, AttemptedAt: row.AttemptedAt,
		ProbeCompletedAt: timeValue(row.ProbeCompletedAt), CleanedAt: timeValue(row.CleanedAt), LastError: row.LastError,
	}
}
