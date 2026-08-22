package membership

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
)

const (
	Version              = 1
	SharedRef            = "refs/heads/dfs-membership"
	PinRef               = "refs/heads/dfs-pins"
	ConfigRef            = "refs/heads/dfs-config"
	privateKeyFile       = "membership-key.pem"
	trustedMembersFile   = "trusted-members.json"
	bootstrapAdminsFile  = "bootstrap-admins.json"
	revokedMembersFile   = "revoked-members.json"
	membersPrefix        = "members/"
	revocationsPrefix    = "revocations/"
	pinsPrefix           = "pins/"
	filesystemConfigPath = "filesystem.json"
)

func KeyPath(repositoryPath string) string {
	return filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), privateKeyFile)
}

type legacyTransport struct {
	Endpoint  string   `json:"endpoint"`
	PublicKey string   `json:"public_key"`
	HostKeys  []string `json:"host_keys"`
}

type Payload struct {
	Version          int              `json:"version"`
	FileSystemID     string           `json:"filesystem_id"`
	PeerID           string           `json:"peer_id"`
	Name             string           `json:"name"`
	Hostname         string           `json:"hostname"`
	Role             string           `json:"role"`
	SigningPublicKey string           `json:"signing_public_key"`
	LegacyTransport  *legacyTransport `json:"ssh,omitempty"`
	QUICEndpoint     string           `json:"quic_endpoint"`
	Generation       uint64           `json:"generation"`
	UpdatedAt        time.Time        `json:"updated_at"`
	Revoked          bool             `json:"revoked"`
}

type Record struct {
	Payload           Payload `json:"payload"`
	SelfSignature     string  `json:"self_signature"`
	ApprovedBy        string  `json:"approved_by"`
	ApprovalSignature string  `json:"approval_signature"`
}

type Endorsement struct {
	PeerID           string `json:"peer_id"`
	SigningPublicKey string `json:"signing_public_key"`
	ApprovedBy       string `json:"approved_by"`
	Signature        string `json:"signature"`
}

type Revocation struct {
	Version      int       `json:"version"`
	FileSystemID string    `json:"filesystem_id"`
	PeerID       string    `json:"peer_id"`
	RevokedBy    string    `json:"revoked_by"`
	Generation   uint64    `json:"generation"`
	UpdatedAt    time.Time `json:"updated_at"`
	Signature    string    `json:"signature"`
}

type PinPolicy struct {
	Version      int       `json:"version"`
	FileSystemID string    `json:"filesystem_id"`
	Path         string    `json:"path"`
	Pinned       bool      `json:"pinned"`
	Generation   uint64    `json:"generation"`
	UpdatedAt    time.Time `json:"updated_at"`
	IssuedBy     string    `json:"issued_by"`
	Signature    string    `json:"signature"`
}

type FilesystemConfig struct {
	Version      int       `json:"version"`
	FileSystemID string    `json:"filesystem_id"`
	Name         string    `json:"name"`
	Generation   uint64    `json:"generation"`
	UpdatedAt    time.Time `json:"updated_at"`
	IssuedBy     string    `json:"issued_by"`
	Signature    string    `json:"signature"`
}

func SetFilesystemName(repositoryPath, filesystemID, peerID, name string) (FilesystemConfig, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 || strings.ContainsAny(name, "\r\n\x00") {
		return FilesystemConfig{}, errors.New("filesystem name must contain 1 to 128 printable characters")
	}
	records, err := Accepted(repositoryPath, filesystemID, peerID)
	if err != nil {
		return FilesystemConfig{}, err
	}
	admin := false
	for _, record := range records {
		if record.Payload.PeerID == peerID && record.Payload.Role == "admin" {
			admin = true
			break
		}
	}
	if !admin {
		return FilesystemConfig{}, errors.New("only an administrator member can rename a DFS filesystem")
	}
	private, public, err := EnsureKey(repositoryPath)
	if err != nil {
		return FilesystemConfig{}, err
	}
	trusted, err := LoadTrusted(repositoryPath)
	if err != nil {
		return FilesystemConfig{}, err
	}
	if trusted[peerID] != public {
		return FilesystemConfig{}, errors.New("local membership signing key does not match trusted membership")
	}
	generation := uint64(1)
	if current, loadErr := LoadFilesystemConfig(repositoryPath, filesystemID); loadErr == nil {
		generation = current.Generation + 1
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return FilesystemConfig{}, fmt.Errorf("load current filesystem configuration: %w", loadErr)
	}
	value := FilesystemConfig{Version: Version, FileSystemID: filesystemID, Name: name, Generation: generation, UpdatedAt: time.Now().UTC(), IssuedBy: peerID}
	value.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, filesystemConfigBytes(value)))
	if err := verifyFilesystemConfig(value, public); err != nil {
		return FilesystemConfig{}, err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return FilesystemConfig{}, err
	}
	data = append(data, '\n')
	if err := writeMetadataFile(repositoryPath, ConfigRef, filesystemConfigPath, data, "filesystem configuration"); err != nil {
		return FilesystemConfig{}, err
	}
	return value, nil
}

func LoadFilesystemConfig(repositoryPath, filesystemID string) (FilesystemConfig, error) {
	files, err := loadSharedPrefix(repositoryPath, ConfigRef, "")
	if err != nil {
		return FilesystemConfig{}, err
	}
	data := files[filesystemConfigPath]
	if len(data) == 0 {
		return FilesystemConfig{}, os.ErrNotExist
	}
	var value FilesystemConfig
	if err := json.Unmarshal(data, &value); err != nil {
		return FilesystemConfig{}, err
	}
	if value.FileSystemID != filesystemID {
		return FilesystemConfig{}, errors.New("filesystem configuration belongs to another filesystem")
	}
	records, err := Accepted(repositoryPath, filesystemID)
	if err != nil {
		return FilesystemConfig{}, err
	}
	for _, record := range records {
		if record.Payload.PeerID == value.IssuedBy && record.Payload.Role == "admin" {
			if err := verifyFilesystemConfig(value, record.Payload.SigningPublicKey); err != nil {
				return FilesystemConfig{}, err
			}
			return value, nil
		}
	}
	return FilesystemConfig{}, errors.New("filesystem configuration was not signed by an accepted administrator")
}

func SetPinPolicy(repositoryPath, filesystemID, peerID, path string, pinned bool) (PinPolicy, error) {
	path, err := normalizePinPath(path)
	if err != nil {
		return PinPolicy{}, err
	}
	private, public, err := EnsureKey(repositoryPath)
	if err != nil {
		return PinPolicy{}, err
	}
	trusted, err := LoadTrusted(repositoryPath)
	if err != nil {
		return PinPolicy{}, err
	}
	if trusted[peerID] != public {
		return PinPolicy{}, errors.New("local pin signing key does not match trusted membership")
	}
	policies, err := LoadPinPolicies(repositoryPath, filesystemID)
	if err != nil {
		return PinPolicy{}, err
	}
	generation := uint64(1)
	for _, policy := range policies {
		if policy.Path == path && policy.Generation >= generation {
			generation = policy.Generation + 1
		}
	}
	policy := PinPolicy{Version: Version, FileSystemID: filesystemID, Path: path, Pinned: pinned,
		Generation: generation, UpdatedAt: time.Now().UTC(), IssuedBy: peerID}
	policy.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, pinPolicyBytes(policy)))
	if err := verifyPinPolicy(policy, public); err != nil {
		return PinPolicy{}, err
	}
	data, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return PinPolicy{}, err
	}
	data = append(data, '\n')
	if err := writePinPolicyFile(repositoryPath, pinPolicyPath(path), data); err != nil {
		return PinPolicy{}, err
	}
	return policy, nil
}

func LoadPinPolicies(repositoryPath, filesystemID string) ([]PinPolicy, error) {
	records, err := Accepted(repositoryPath, filesystemID)
	if err != nil {
		return nil, err
	}
	trusted := make(map[string]string, len(records))
	for _, record := range records {
		trusted[record.Payload.PeerID] = record.Payload.SigningPublicKey
	}
	files, err := loadSharedPrefix(repositoryPath, PinRef, pinsPrefix)
	if err != nil {
		return nil, err
	}
	var policies []PinPolicy
	for path, data := range files {
		var policy PinPolicy
		if json.Unmarshal(data, &policy) != nil || policy.FileSystemID != filesystemID || path != pinPolicyPath(policy.Path) {
			continue
		}
		public := trusted[policy.IssuedBy]
		if public == "" || verifyPinPolicy(policy, public) != nil {
			continue
		}
		policies = append(policies, policy)
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].Path < policies[j].Path })
	return policies, nil
}

func ActivePinPaths(repositoryPath, filesystemID string) ([]string, error) {
	policies, err := LoadPinPolicies(repositoryPath, filesystemID)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, policy := range policies {
		if policy.Pinned {
			paths = append(paths, policy.Path)
		}
	}
	return paths, nil
}

func Revoke(filesystemID, peerID, revokedBy string, generation uint64, private ed25519.PrivateKey) (Revocation, error) {
	revocation := Revocation{Version: Version, FileSystemID: filesystemID, PeerID: peerID, RevokedBy: revokedBy,
		Generation: generation, UpdatedAt: time.Now().UTC()}
	if err := validateRevocation(revocation); err != nil {
		return Revocation{}, err
	}
	revocation.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, revocationBytes(revocation)))
	return revocation, nil
}

func VerifyRevocation(revocation Revocation, approver Record) error {
	if err := validateRevocation(revocation); err != nil {
		return err
	}
	if revocation.RevokedBy != approver.Payload.PeerID || approver.Payload.Role != "admin" {
		return errors.New("membership revocation was not issued by an administrator")
	}
	public, err := decodePublicKey(approver.Payload.SigningPublicKey)
	if err != nil {
		return err
	}
	signature, err := base64.RawStdEncoding.DecodeString(revocation.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(public, revocationBytes(revocation), signature) {
		return errors.New("invalid membership revocation signature")
	}
	return nil
}

func SaveRevocation(repositoryPath string, revocation Revocation) error {
	if err := validateRevocation(revocation); err != nil {
		return err
	}
	data, err := json.MarshalIndent(revocation, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeSharedFile(repositoryPath, revocationsPrefix+revocation.PeerID+".json", data)
}

func Endorse(record Record, approverID string, private ed25519.PrivateKey) (Endorsement, error) {
	if err := VerifySelf(record); err != nil {
		return Endorsement{}, err
	}
	endorsement := Endorsement{PeerID: record.Payload.PeerID, SigningPublicKey: record.Payload.SigningPublicKey, ApprovedBy: approverID}
	endorsement.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, endorsementBytes(endorsement)))
	return endorsement, nil
}

func VerifyEndorsement(endorsement Endorsement, approver Record) error {
	if endorsement.ApprovedBy != approver.Payload.PeerID || !validPeerID(endorsement.PeerID) {
		return errors.New("membership endorsement references an invalid peer")
	}
	if _, err := decodePublicKey(endorsement.SigningPublicKey); err != nil {
		return err
	}
	signature, err := base64.RawStdEncoding.DecodeString(endorsement.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("invalid membership endorsement encoding")
	}
	public, err := decodePublicKey(approver.Payload.SigningPublicKey)
	if err != nil {
		return err
	}
	if !ed25519.Verify(public, endorsementBytes(endorsement), signature) {
		return errors.New("invalid membership endorsement signature")
	}
	return nil
}

func EnsureKey(repositoryPath string) (ed25519.PrivateKey, string, error) {
	directory := filepath.Join(repositoryPath, filepath.FromSlash(config.Directory))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, "", err
	}
	path := KeyPath(repositoryPath)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		public, private, generateErr := ed25519.GenerateKey(rand.Reader)
		if generateErr != nil {
			return nil, "", generateErr
		}
		encoded, marshalErr := x509.MarshalPKCS8PrivateKey(private)
		if marshalErr != nil {
			return nil, "", marshalErr
		}
		data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
		if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
			return nil, "", writeErr
		}
		return private, base64.RawStdEncoding.EncodeToString(public), nil
	}
	if err != nil {
		return nil, "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, "", errors.New("decode DFS membership private key: invalid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("decode DFS membership private key: %w", err)
	}
	private, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, "", errors.New("DFS membership private key is not Ed25519")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, "", err
	}
	return private, base64.RawStdEncoding.EncodeToString(private.Public().(ed25519.PublicKey)), nil
}

func Sign(payload Payload, private ed25519.PrivateKey) (Record, error) {
	if err := validatePayload(payload); err != nil {
		return Record{}, err
	}
	data, err := canonicalPayload(payload)
	if err != nil {
		return Record{}, err
	}
	record := Record{Payload: payload, SelfSignature: base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, data))}
	return record, nil
}

func Approve(record Record, approverID string, private ed25519.PrivateKey) (Record, error) {
	if err := VerifySelf(record); err != nil {
		return Record{}, err
	}
	approverID = strings.TrimSpace(approverID)
	if approverID == "" {
		return Record{}, errors.New("membership approver ID is empty")
	}
	record.ApprovedBy = approverID
	data, err := approvalBytes(record)
	if err != nil {
		return Record{}, err
	}
	record.ApprovalSignature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, data))
	return record, nil
}

func VerifySelf(record Record) error {
	if err := validatePayload(record.Payload); err != nil {
		return err
	}
	public, err := decodePublicKey(record.Payload.SigningPublicKey)
	if err != nil {
		return err
	}
	signature, err := base64.RawStdEncoding.DecodeString(record.SelfSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("invalid membership self-signature encoding")
	}
	data, err := canonicalPayload(record.Payload)
	if err != nil {
		return err
	}
	if !ed25519.Verify(public, data, signature) {
		return errors.New("invalid membership self-signature")
	}
	return nil
}

func VerifyApproval(record, approver Record) error {
	if err := VerifySelf(record); err != nil {
		return err
	}
	if err := VerifySelf(approver); err != nil {
		return fmt.Errorf("invalid approver record: %w", err)
	}
	if record.ApprovedBy != approver.Payload.PeerID {
		return errors.New("membership approval references a different peer")
	}
	public, err := decodePublicKey(approver.Payload.SigningPublicKey)
	if err != nil {
		return err
	}
	signature, err := base64.RawStdEncoding.DecodeString(record.ApprovalSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("invalid membership approval encoding")
	}
	data, err := approvalBytes(record)
	if err != nil {
		return err
	}
	if !ed25519.Verify(public, data, signature) {
		return errors.New("invalid membership approval signature")
	}
	return nil
}

func Save(repositoryPath string, record Record) error {
	if err := VerifySelf(record); err != nil {
		return err
	}
	if record.ApprovedBy == "" || record.ApprovalSignature == "" {
		return errors.New("membership record is not approved")
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeSharedFile(repositoryPath, membersPrefix+record.Payload.PeerID+".json", data)
}

func LoadAll(repositoryPath string) ([]Record, error) {
	files, err := loadSharedPrefix(repositoryPath, SharedRef, membersPrefix)
	if err != nil {
		return nil, err
	}
	var records []Record
	for path, data := range files {
		name := strings.TrimPrefix(path, membersPrefix)
		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("decode membership %s: %w", name, err)
		}
		if record.Payload.PeerID+".json" != name {
			return nil, fmt.Errorf("membership filename does not match peer ID: %s", name)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Payload.PeerID < records[j].Payload.PeerID })
	return records, nil
}

func Trust(repositoryPath, peerID, signingPublicKey string) error {
	trusted, err := LoadTrusted(repositoryPath)
	if err != nil {
		return err
	}
	peerID = strings.TrimSpace(peerID)
	if !validPeerID(peerID) {
		return errors.New("invalid trusted membership peer ID")
	}
	if signingPublicKey != "" {
		if _, err := decodePublicKey(signingPublicKey); err != nil {
			return err
		}
	}
	existing, found := trusted[peerID]
	if existing != "" && signingPublicKey != "" && existing != signingPublicKey {
		return errors.New("membership signing key does not match the trusted key")
	}
	if found && (signingPublicKey == "" || existing == signingPublicKey) {
		return nil
	}
	trusted[peerID] = signingPublicKey
	data, err := json.MarshalIndent(trusted, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), trustedMembersFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return replacePrivateFile(path, data)
}

func LoadTrusted(repositoryPath string) (map[string]string, error) {
	path := filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), trustedMembersFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, err
	}
	trusted := make(map[string]string)
	if err := json.Unmarshal(data, &trusted); err != nil {
		return nil, err
	}
	return trusted, nil
}

// TrustStateVersion fingerprints the small local inputs used to authorize and
// route managed peers. Callers can cheaply reuse a validated membership record
// while this value is unchanged without spawning Git on every content stream.
func TrustStateVersion(repositoryPath string) (string, error) {
	paths := []string{
		filepath.Join(repositoryPath, ".git", "refs", "heads", "dfs-membership"),
		filepath.Join(repositoryPath, ".git", "packed-refs"),
		filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), trustedMembersFile),
		filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), bootstrapAdminsFile),
		filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), revokedMembersFile),
	}
	hash := sha256.New()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, _ = hash.Write([]byte(path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
	}
	return base64.RawStdEncoding.EncodeToString(hash.Sum(nil)), nil
}

// IsRevoked checks the locally persisted set of cryptographically accepted
// membership revocations. The set is refreshed whenever Accepted processes
// shared membership state and is safe to consult on latency-sensitive paths.
func IsRevoked(repositoryPath, peerID string) (bool, error) {
	revoked, err := loadRevoked(repositoryPath)
	if err != nil {
		return false, err
	}
	return revoked[peerID], nil
}

// Accepted returns the non-revoked records whose approval chain reaches a
// locally pinned, self-approved bootstrap administrator. Every subsequent
// admission and role change must be approved by an accepted administrator.
func Accepted(repositoryPath, filesystemID string, legacyPeerIDs ...string) ([]Record, error) {
	for _, peerID := range legacyPeerIDs {
		if err := Trust(repositoryPath, peerID, ""); err != nil {
			return nil, err
		}
	}
	trusted, err := LoadTrusted(repositoryPath)
	if err != nil {
		return nil, err
	}
	records, err := LoadAll(repositoryPath)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Record)
	for _, record := range records {
		if record.Payload.FileSystemID == filesystemID && VerifySelf(record) == nil {
			byID[record.Payload.PeerID] = record
		}
	}
	for id, key := range trusted {
		if key == "" {
			if record, ok := byID[id]; ok {
				trusted[id] = record.Payload.SigningPublicKey
			}
		}
	}
	bootstrap, err := loadBootstrapAdmins(repositoryPath)
	if err != nil {
		return nil, err
	}
	if len(bootstrap) == 0 {
		// One-time migration for repositories created before bootstrap roots
		// were stored separately. Only a pinned generation-one administrator
		// with a valid self-approval can seed the authority chain.
		for id, record := range byID {
			if trusted[id] == record.Payload.SigningPublicKey && record.Payload.Generation == 1 &&
				record.Payload.Role == "admin" && record.ApprovedBy == id && VerifyApproval(record, record) == nil {
				bootstrap[id] = record.Payload.SigningPublicKey
			}
		}
		if len(bootstrap) > 0 {
			if err := saveBootstrapAdmins(repositoryPath, bootstrap); err != nil {
				return nil, err
			}
		}
	}
	persistedRevoked, err := loadRevoked(repositoryPath)
	if err != nil {
		return nil, err
	}
	authorized := authorizeRecords(byID, bootstrap, persistedRevoked)
	revoked, err := acceptedRevocations(repositoryPath, filesystemID, byID, authorized)
	if err != nil {
		return nil, err
	}
	revocationsChanged := false
	for id := range revoked {
		if !persistedRevoked[id] {
			persistedRevoked[id] = true
			revocationsChanged = true
		}
	}
	if revocationsChanged {
		if err := saveRevoked(repositoryPath, persistedRevoked); err != nil {
			return nil, err
		}
	}
	revoked = persistedRevoked
	authorized = authorizeRecords(byID, bootstrap, revoked)
	var accepted []Record
	for id, key := range authorized {
		if record, ok := byID[id]; ok && record.Payload.SigningPublicKey == key && !record.Payload.Revoked && !revoked[id] {
			accepted = append(accepted, record)
		}
	}
	for id, key := range authorized {
		if key != "" {
			if err := Trust(repositoryPath, id, key); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(accepted, func(i, j int) bool { return accepted[i].Payload.PeerID < accepted[j].Payload.PeerID })
	return accepted, nil
}

// TrustBootstrapAdmin pins an administrator delivered through an authenticated
// bootstrap path, such as initial filesystem creation or pairing. The local pin
// is the root of the later signed approval chain.
func TrustBootstrapAdmin(repositoryPath string, record Record) error {
	if err := VerifySelf(record); err != nil {
		return err
	}
	if record.Payload.Role != "admin" {
		return errors.New("bootstrap membership is not an administrator")
	}
	if err := Trust(repositoryPath, record.Payload.PeerID, record.Payload.SigningPublicKey); err != nil {
		return err
	}
	bootstrap, err := loadBootstrapAdmins(repositoryPath)
	if err != nil {
		return err
	}
	if key := bootstrap[record.Payload.PeerID]; key != "" && key != record.Payload.SigningPublicKey {
		return errors.New("bootstrap administrator key replacement was rejected")
	}
	bootstrap[record.Payload.PeerID] = record.Payload.SigningPublicKey
	return saveBootstrapAdmins(repositoryPath, bootstrap)
}

func authorizeRecords(byID map[string]Record, bootstrap map[string]string, revoked map[string]bool) map[string]string {
	authorized := make(map[string]string)
	for id, key := range bootstrap {
		record, ok := byID[id]
		if !ok || key == "" || record.Payload.SigningPublicKey != key || record.Payload.Role != "admin" ||
			revoked[id] || VerifySelf(record) != nil {
			continue
		}
		authorized[id] = key
	}
	for changed := true; changed; {
		changed = false
		for id, record := range byID {
			if authorized[id] != "" || revoked[id] || revoked[record.ApprovedBy] {
				continue
			}
			approver, ok := byID[record.ApprovedBy]
			if !ok || approver.Payload.Role != "admin" || approver.Payload.SigningPublicKey != authorized[record.ApprovedBy] {
				continue
			}
			if VerifyApproval(record, approver) == nil {
				authorized[id] = record.Payload.SigningPublicKey
				changed = true
			}
		}
	}
	return authorized
}

func loadBootstrapAdmins(repositoryPath string) (map[string]string, error) {
	path := filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), bootstrapAdminsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func saveBootstrapAdmins(repositoryPath string, roots map[string]string) error {
	data, err := json.MarshalIndent(roots, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), bootstrapAdminsFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return replacePrivateFile(path, data)
}

// CurrentMachines collapses accepted records left behind by peer reinstalls to
// the newest identity for each named machine. A reinstalled host keeps its
// hostname and peer name but receives a new peer ID and signing key.
func CurrentMachines(records []Record) ([]Record, map[string]bool) {
	selected := make(map[string]Record, len(records))
	for _, record := range records {
		hostname := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(record.Payload.Hostname)), ".")
		if hostname == "" {
			hostname = "peer-id:" + record.Payload.PeerID
		}
		name := strings.ToLower(strings.TrimSpace(record.Payload.Name))
		machine := hostname + "\x00" + name
		current, found := selected[machine]
		if !found || record.Payload.UpdatedAt.After(current.Payload.UpdatedAt) ||
			(record.Payload.UpdatedAt.Equal(current.Payload.UpdatedAt) && record.Payload.PeerID > current.Payload.PeerID) {
			selected[machine] = record
		}
	}
	current := make([]Record, 0, len(selected))
	active := make(map[string]bool, len(selected))
	for _, record := range selected {
		current = append(current, record)
		active[record.Payload.PeerID] = true
	}
	sort.Slice(current, func(i, j int) bool { return current[i].Payload.PeerID < current[j].Payload.PeerID })
	superseded := make(map[string]bool)
	for _, record := range records {
		if !active[record.Payload.PeerID] {
			superseded[record.Payload.PeerID] = true
		}
	}
	return current, superseded
}

func AcceptedRevocations(repositoryPath, filesystemID string) (map[string]bool, error) {
	if _, err := Accepted(repositoryPath, filesystemID); err != nil {
		return nil, err
	}
	return loadRevoked(repositoryPath)
}

func canonicalPayload(payload Payload) ([]byte, error) { return json.Marshal(payload) }

func approvalBytes(record Record) ([]byte, error) {
	payload, err := canonicalPayload(record.Payload)
	if err != nil {
		return nil, err
	}
	return append(append(payload, '\n'), record.SelfSignature...), nil
}

func endorsementBytes(endorsement Endorsement) []byte {
	return []byte(endorsement.PeerID + "\n" + endorsement.SigningPublicKey + "\n" + endorsement.ApprovedBy)
}

func revocationBytes(revocation Revocation) []byte {
	copy := revocation
	copy.Signature = ""
	data, _ := json.Marshal(copy)
	return data
}

func pinPolicyBytes(policy PinPolicy) []byte {
	copy := policy
	copy.Signature = ""
	data, _ := json.Marshal(copy)
	return data
}

func filesystemConfigBytes(value FilesystemConfig) []byte {
	return []byte(fmt.Sprintf("%d\x00%s\x00%s\x00%d\x00%s\x00%s", value.Version, value.FileSystemID, value.Name,
		value.Generation, value.UpdatedAt.UTC().Format(time.RFC3339Nano), value.IssuedBy))
}

func verifyFilesystemConfig(value FilesystemConfig, encodedPublic string) error {
	if value.Version != Version || len(value.FileSystemID) < 16 || strings.TrimSpace(value.Name) == "" || len(value.Name) > 128 ||
		strings.ContainsAny(value.Name, "\r\n\x00") || !validPeerID(value.IssuedBy) || value.Generation == 0 || value.UpdatedAt.IsZero() {
		return errors.New("incomplete filesystem configuration")
	}
	public, err := decodePublicKey(encodedPublic)
	if err != nil {
		return err
	}
	signature, err := base64.RawStdEncoding.DecodeString(value.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(public, filesystemConfigBytes(value), signature) {
		return errors.New("invalid filesystem configuration signature")
	}
	return nil
}

func verifyPinPolicy(policy PinPolicy, encodedPublic string) error {
	if policy.Version != Version || len(policy.FileSystemID) < 16 || !validPeerID(policy.IssuedBy) || policy.Generation == 0 || policy.UpdatedAt.IsZero() {
		return errors.New("incomplete cluster pin policy")
	}
	normalized, err := normalizePinPath(policy.Path)
	if err != nil || normalized != policy.Path {
		return errors.New("invalid cluster pin path")
	}
	public, err := decodePublicKey(encodedPublic)
	if err != nil {
		return err
	}
	signature, err := base64.RawStdEncoding.DecodeString(policy.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(public, pinPolicyBytes(policy), signature) {
		return errors.New("invalid cluster pin policy signature")
	}
	return nil
}

func normalizePinPath(path string) (string, error) {
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	path = strings.TrimPrefix(path, "./")
	if path == "." {
		path = ""
	}
	if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
		return "", errors.New("pin path must be relative to the DFS root")
	}
	return strings.TrimPrefix(path, "/"), nil
}

func pinPolicyPath(path string) string {
	digest := sha256.Sum256([]byte(path))
	return fmt.Sprintf("%s%x.json", pinsPrefix, digest[:])
}

func validateRevocation(revocation Revocation) error {
	if revocation.Version != Version || len(revocation.FileSystemID) < 16 || !validPeerID(revocation.PeerID) ||
		!validPeerID(revocation.RevokedBy) || revocation.Generation == 0 || revocation.UpdatedAt.IsZero() {
		return errors.New("incomplete membership revocation")
	}
	return nil
}

func acceptedRevocations(repositoryPath, filesystemID string, records map[string]Record, trusted map[string]string) (map[string]bool, error) {
	files, err := loadSharedPrefix(repositoryPath, SharedRef, revocationsPrefix)
	if err != nil {
		return nil, err
	}
	var candidates []Revocation
	revokedAt := make(map[string]time.Time)
	for path, data := range files {
		name := strings.TrimPrefix(path, revocationsPrefix)
		var revocation Revocation
		if json.Unmarshal(data, &revocation) != nil || revocation.FileSystemID != filesystemID || revocation.PeerID+".json" != name {
			continue
		}
		approver, ok := records[revocation.RevokedBy]
		if ok && approver.Payload.SigningPublicKey == trusted[revocation.RevokedBy] && VerifyRevocation(revocation, approver) == nil {
			candidates = append(candidates, revocation)
			if current, found := revokedAt[revocation.PeerID]; !found || revocation.UpdatedAt.Before(current) {
				revokedAt[revocation.PeerID] = revocation.UpdatedAt
			}
		}
	}
	result := make(map[string]bool)
	for _, revocation := range candidates {
		// Preserve revocations issued before an administrator was removed, but
		// reject claims issued at or after that administrator's revocation.
		issuerRevokedAt, issuerRevoked := revokedAt[revocation.RevokedBy]
		if !issuerRevoked || revocation.UpdatedAt.Before(issuerRevokedAt) {
			result[revocation.PeerID] = true
		}
	}
	return result, nil
}

func loadRevoked(repositoryPath string) (map[string]bool, error) {
	path := filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), revokedMembersFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]bool), nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool)
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func saveRevoked(repositoryPath string, revoked map[string]bool) error {
	path := filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), revokedMembersFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(revoked, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return replacePrivateFile(path, data)
}

func replacePrivateFile(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.new")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func decodePublicKey(value string) (ed25519.PublicKey, error) {
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("invalid membership signing public key")
	}
	return ed25519.PublicKey(decoded), nil
}

func validatePayload(payload Payload) error {
	if payload.Version != Version || len(payload.FileSystemID) < 16 || !validPeerID(payload.PeerID) ||
		strings.TrimSpace(payload.Name) == "" || strings.TrimSpace(payload.Hostname) == "" ||
		(payload.Role != "admin" && payload.Role != "member") || payload.Generation == 0 || payload.UpdatedAt.IsZero() {
		return errors.New("incomplete membership payload")
	}
	if _, err := decodePublicKey(payload.SigningPublicKey); err != nil {
		return err
	}
	if payload.LegacyTransport != nil {
		endpoint := strings.TrimSpace(payload.LegacyTransport.Endpoint)
		publicKey := strings.TrimSpace(payload.LegacyTransport.PublicKey)
		if (endpoint == "") != (publicKey == "") {
			return errors.New("legacy membership transport is incomplete")
		}
	}
	if !strings.HasPrefix(payload.QUICEndpoint, "quic://") {
		return errors.New("membership QUIC transport is incomplete")
	}
	return nil
}

func validPeerID(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

// MigrateLegacySharedState moves control-plane records out of the worktree and
// into a dedicated Git ref. Only the two historical DFS directories and their
// exact annex attributes are touched; unrelated user attributes are retained.
func MigrateLegacySharedState(repositoryPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runGit(ctx, repositoryPath, nil, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("locate Git metadata for membership migration: %w", err)
	}
	for _, item := range []struct {
		directory string
		prefix    string
	}{
		{filepath.Join(repositoryPath, ".dfs", "members"), membersPrefix},
		{filepath.Join(repositoryPath, ".dfs", "revocations"), revocationsPrefix},
	} {
		entries, err := os.ReadDir(item.directory)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(item.directory, entry.Name()))
			if err != nil {
				return err
			}
			if err := writeSharedFile(repositoryPath, item.prefix+entry.Name(), data); err != nil {
				return fmt.Errorf("import legacy membership metadata %s: %w", entry.Name(), err)
			}
		}
	}
	if _, err := runGit(ctx, repositoryPath, nil, "rm", "-r", "-f", "--ignore-unmatch", "--", ".dfs/members", ".dfs/revocations"); err != nil {
		return fmt.Errorf("remove legacy membership files from worktree: %w", err)
	}
	attributesPath := filepath.Join(repositoryPath, ".gitattributes")
	attributes, err := os.ReadFile(attributesPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		var retained []string
		changed := false
		for _, line := range strings.Split(string(attributes), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == ".dfs/members/** annex.largefiles=nothing" || trimmed == ".dfs/revocations/** annex.largefiles=nothing" {
				changed = true
				continue
			}
			retained = append(retained, line)
		}
		if changed {
			data := []byte(strings.TrimRight(strings.Join(retained, "\n"), "\n"))
			if len(data) > 0 {
				data = append(data, '\n')
				if err := os.WriteFile(attributesPath, data, 0o644); err != nil {
					return err
				}
				if _, err := runGit(ctx, repositoryPath, nil, "add", "--", ".gitattributes"); err != nil {
					return err
				}
			} else {
				if err := os.Remove(attributesPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				if _, err := runGit(ctx, repositoryPath, nil, "add", "-A", "--", ".gitattributes"); err != nil {
					return err
				}
			}
		}
	}
	staged, err := runGit(ctx, repositoryPath, nil, "diff", "--cached", "--name-only", "--", ".dfs/members", ".dfs/revocations", ".gitattributes")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(staged)) != "" {
		arguments := []string{"commit", "-m", "Move DFS membership into Git metadata", "--"}
		arguments = append(arguments, strings.Fields(string(staged))...)
		if _, err := runGit(ctx, repositoryPath, nil, arguments...); err != nil {
			return fmt.Errorf("commit membership metadata migration: %w", err)
		}
	}
	_ = os.Remove(filepath.Join(repositoryPath, ".dfs", "members"))
	_ = os.Remove(filepath.Join(repositoryPath, ".dfs", "revocations"))
	_ = os.Remove(filepath.Join(repositoryPath, ".dfs"))
	return nil
}

// Sync exchanges the dedicated membership ref with configured Git remotes.
// Records are merged by signed generation and the resulting commit has every
// fetched ref as a parent, allowing ordinary fast-forward pushes.
func Sync(ctx context.Context, repositoryPath string, remotes []string) error {
	refs := []string{SharedRef}
	var fetched []struct {
		remote string
		ref    string
	}
	for _, remote := range remotes {
		remote = strings.TrimSpace(remote)
		if remote == "" {
			continue
		}
		digest := sha256.Sum256([]byte(remote))
		tracking := fmt.Sprintf("refs/dfs/remotes/%x", digest[:8])
		if _, err := runGit(ctx, repositoryPath, nil, "fetch", "--no-tags", remote, "+"+SharedRef+":"+tracking); err != nil {
			continue
		}
		refs = append(refs, tracking)
		fetched = append(fetched, struct {
			remote string
			ref    string
		}{remote: remote, ref: tracking})
	}
	trusted, err := LoadTrusted(repositoryPath)
	if err != nil {
		return err
	}
	merged := make(map[string][]byte)
	for _, ref := range refs {
		files, err := loadSharedPrefix(repositoryPath, ref, "")
		if err != nil {
			return err
		}
		for path, data := range files {
			if !validSharedDocument(path, data, trusted) {
				continue
			}
			if current, exists := merged[path]; !exists || newerSharedDocument(path, data, current) {
				merged[path] = data
			}
		}
	}
	for path, data := range merged {
		if err := writeSharedFile(repositoryPath, path, data); err != nil {
			return err
		}
	}
	remoteRefs := make([]string, 0, len(fetched))
	for _, item := range fetched {
		remoteRefs = append(remoteRefs, item.ref)
	}
	if err := joinSharedHistories(ctx, repositoryPath, remoteRefs); err != nil {
		return err
	}
	for _, item := range fetched {
		_, _ = runGit(ctx, repositoryPath, nil, "push", item.remote, SharedRef+":"+SharedRef)
	}
	if err := syncFilesystemConfig(ctx, repositoryPath, remotes); err != nil {
		return err
	}
	pinSigners, err := acceptedSigningKeys(repositoryPath)
	if err != nil {
		return err
	}
	return syncPinPolicies(ctx, repositoryPath, remotes, pinSigners)
}

func acceptedSigningKeys(repositoryPath string) (map[string]string, error) {
	records, err := LoadAll(repositoryPath)
	if err != nil {
		return nil, err
	}
	filesystems := make(map[string]bool)
	for _, record := range records {
		filesystems[record.Payload.FileSystemID] = true
	}
	result := make(map[string]string)
	for filesystemID := range filesystems {
		accepted, err := Accepted(repositoryPath, filesystemID)
		if err != nil {
			return nil, err
		}
		for _, record := range accepted {
			result[record.Payload.PeerID] = record.Payload.SigningPublicKey
		}
	}
	return result, nil
}

func syncFilesystemConfig(ctx context.Context, repositoryPath string, remotes []string) error {
	cfg, err := config.Load(repositoryPath)
	if err != nil {
		// Bare repositories used for membership exchange, and repositories from
		// versions predating replicated filesystem configuration, have nothing
		// to reconcile yet.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	records, err := Accepted(repositoryPath, cfg.FileSystemID, cfg.PeerID)
	if err != nil {
		return err
	}
	adminKeys := make(map[string]string)
	for _, record := range records {
		if record.Payload.Role == "admin" {
			adminKeys[record.Payload.PeerID] = record.Payload.SigningPublicKey
		}
	}
	refs := []string{ConfigRef}
	var fetched []struct{ remote, ref string }
	for _, remote := range remotes {
		remote = strings.TrimSpace(remote)
		if remote == "" {
			continue
		}
		digest := sha256.Sum256([]byte(remote))
		tracking := fmt.Sprintf("refs/dfs/config-remotes/%x", digest[:8])
		if _, err := runGit(ctx, repositoryPath, nil, "fetch", "--no-tags", remote, "+"+ConfigRef+":"+tracking); err != nil {
			continue
		}
		refs = append(refs, tracking)
		fetched = append(fetched, struct{ remote, ref string }{remote: remote, ref: tracking})
	}
	var selected FilesystemConfig
	var selectedData []byte
	for _, ref := range refs {
		files, err := loadSharedPrefix(repositoryPath, ref, "")
		if err != nil {
			return err
		}
		data := files[filesystemConfigPath]
		var candidate FilesystemConfig
		if len(data) == 0 || json.Unmarshal(data, &candidate) != nil || candidate.FileSystemID != cfg.FileSystemID ||
			adminKeys[candidate.IssuedBy] == "" || verifyFilesystemConfig(candidate, adminKeys[candidate.IssuedBy]) != nil {
			continue
		}
		if selectedData == nil || candidate.Generation > selected.Generation ||
			(candidate.Generation == selected.Generation && candidate.UpdatedAt.After(selected.UpdatedAt)) {
			selected, selectedData = candidate, data
		}
	}
	if selectedData != nil {
		if err := writeMetadataFile(repositoryPath, ConfigRef, filesystemConfigPath, selectedData, "filesystem configuration"); err != nil {
			return err
		}
	}
	remoteRefs := make([]string, 0, len(fetched))
	for _, item := range fetched {
		remoteRefs = append(remoteRefs, item.ref)
	}
	if err := joinRefHistories(ctx, repositoryPath, ConfigRef, remoteRefs, "filesystem configuration"); err != nil {
		return err
	}
	for _, remote := range remotes {
		remote = strings.TrimSpace(remote)
		if remote != "" {
			_, _ = runGit(ctx, repositoryPath, nil, "push", remote, ConfigRef+":"+ConfigRef)
		}
	}
	return nil
}

func syncPinPolicies(ctx context.Context, repositoryPath string, remotes []string, trusted map[string]string) error {
	refs := []string{PinRef}
	var fetched []struct{ remote, ref string }
	for _, remote := range remotes {
		remote = strings.TrimSpace(remote)
		if remote == "" {
			continue
		}
		digest := sha256.Sum256([]byte(remote))
		tracking := fmt.Sprintf("refs/dfs/pin-remotes/%x", digest[:8])
		if _, err := runGit(ctx, repositoryPath, nil, "fetch", "--no-tags", remote, "+"+PinRef+":"+tracking); err != nil {
			continue
		}
		refs = append(refs, tracking)
		fetched = append(fetched, struct{ remote, ref string }{remote: remote, ref: tracking})
	}
	merged := make(map[string][]byte)
	for _, ref := range refs {
		files, err := loadSharedPrefix(repositoryPath, ref, pinsPrefix)
		if err != nil {
			return err
		}
		for path, data := range files {
			if !validPinDocument(path, data, trusted) {
				continue
			}
			if current, exists := merged[path]; !exists || newerSharedDocument(path, data, current) {
				merged[path] = data
			}
		}
	}
	for path, data := range merged {
		if err := writePinPolicyFile(repositoryPath, path, data); err != nil {
			return err
		}
	}
	remoteRefs := make([]string, 0, len(fetched))
	for _, item := range fetched {
		remoteRefs = append(remoteRefs, item.ref)
	}
	if err := joinRefHistories(ctx, repositoryPath, PinRef, remoteRefs, "cluster pin"); err != nil {
		return err
	}
	for _, remote := range remotes {
		remote = strings.TrimSpace(remote)
		if remote != "" {
			_, _ = runGit(ctx, repositoryPath, nil, "push", remote, PinRef+":"+PinRef)
		}
	}
	return nil
}

func writeSharedFile(repositoryPath, path string, data []byte) error {
	if !strings.HasPrefix(path, membersPrefix) && !strings.HasPrefix(path, revocationsPrefix) {
		return errors.New("invalid DFS membership metadata path")
	}
	return writeMetadataFile(repositoryPath, SharedRef, path, data, "membership")
}

func writePinPolicyFile(repositoryPath, path string, data []byte) error {
	if !strings.HasPrefix(path, pinsPrefix) {
		return errors.New("invalid DFS cluster pin metadata path")
	}
	return writeMetadataFile(repositoryPath, PinRef, path, data, "cluster pin")
}

func writeMetadataFile(repositoryPath, ref, path string, data []byte, kind string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for attempt := 0; attempt < 5; attempt++ {
		old, _ := resolveRef(ctx, repositoryPath, ref)
		if old != "" {
			if existing, err := runGit(ctx, repositoryPath, nil, "show", ref+":"+path); err == nil && bytes.Equal(existing, data) {
				return nil
			}
		}
		gitDirectory, err := runGit(ctx, repositoryPath, nil, "rev-parse", "--git-dir")
		if err != nil {
			return err
		}
		gitPath := strings.TrimSpace(string(gitDirectory))
		if !filepath.IsAbs(gitPath) {
			gitPath = filepath.Join(repositoryPath, gitPath)
		}
		if err := os.MkdirAll(filepath.Join(gitPath, "dfs"), 0o700); err != nil {
			return err
		}
		index, err := os.CreateTemp(filepath.Join(gitPath, "dfs"), "metadata-index-*")
		if err != nil {
			return err
		}
		indexPath := index.Name()
		_ = index.Close()
		_ = os.Remove(indexPath)
		environment := []string{"GIT_INDEX_FILE=" + indexPath}
		if old == "" {
			_, err = runGitEnv(ctx, repositoryPath, environment, nil, "read-tree", "--empty")
		} else {
			_, err = runGitEnv(ctx, repositoryPath, environment, nil, "read-tree", old+"^{tree}")
		}
		if err == nil {
			var blob []byte
			blob, err = runGitEnv(ctx, repositoryPath, environment, data, "hash-object", "-w", "--stdin")
			if err == nil {
				_, err = runGitEnv(ctx, repositoryPath, environment, nil, "update-index", "--add", "--cacheinfo", "100644", strings.TrimSpace(string(blob)), path)
			}
		}
		var tree []byte
		if err == nil {
			tree, err = runGitEnv(ctx, repositoryPath, environment, nil, "write-tree")
		}
		_ = os.Remove(indexPath)
		if err != nil {
			return err
		}
		arguments := []string{"commit-tree", strings.TrimSpace(string(tree)), "-m", "Update DFS " + kind + " metadata"}
		if old != "" {
			arguments = append(arguments, "-p", old)
		}
		commit, err := runGit(ctx, repositoryPath, nil, arguments...)
		if err != nil {
			return err
		}
		if _, err := runGit(ctx, repositoryPath, nil, "update-ref", ref, strings.TrimSpace(string(commit)), old); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s metadata changed concurrently too many times", kind)
}

func removeSharedFile(repositoryPath, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	old, err := resolveRef(ctx, repositoryPath, SharedRef)
	if err != nil || old == "" {
		return err
	}
	gitDirectory, err := runGit(ctx, repositoryPath, nil, "rev-parse", "--git-dir")
	if err != nil {
		return err
	}
	gitPath := strings.TrimSpace(string(gitDirectory))
	if !filepath.IsAbs(gitPath) {
		gitPath = filepath.Join(repositoryPath, gitPath)
	}
	index, err := os.CreateTemp(gitPath, "membership-index-*")
	if err != nil {
		return err
	}
	indexPath := index.Name()
	_ = index.Close()
	_ = os.Remove(indexPath)
	defer os.Remove(indexPath)
	environment := []string{"GIT_INDEX_FILE=" + indexPath}
	if _, err := runGitEnv(ctx, repositoryPath, environment, nil, "read-tree", old+"^{tree}"); err != nil {
		return err
	}
	if _, err := runGitEnv(ctx, repositoryPath, environment, nil, "update-index", "--force-remove", "--", path); err != nil {
		return err
	}
	tree, err := runGitEnv(ctx, repositoryPath, environment, nil, "write-tree")
	if err != nil {
		return err
	}
	commit, err := runGit(ctx, repositoryPath, nil, "commit-tree", strings.TrimSpace(string(tree)), "-p", old, "-m", "Update DFS membership metadata")
	if err != nil {
		return err
	}
	_, err = runGit(ctx, repositoryPath, nil, "update-ref", SharedRef, strings.TrimSpace(string(commit)), old)
	return err
}

func loadSharedPrefix(repositoryPath, ref, prefix string) (map[string][]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if value, _ := resolveRef(ctx, repositoryPath, ref); value == "" {
		return make(map[string][]byte), nil
	}
	arguments := []string{"ls-tree", "-r", "--name-only", ref}
	if prefix != "" {
		arguments = append(arguments, "--", strings.TrimSuffix(prefix, "/"))
	}
	listing, err := runGit(ctx, repositoryPath, nil, arguments...)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]byte)
	for _, path := range strings.Split(strings.TrimSpace(string(listing)), "\n") {
		if path == "" || (prefix != "" && !strings.HasPrefix(path, prefix)) {
			continue
		}
		data, err := runGit(ctx, repositoryPath, nil, "show", ref+":"+path)
		if err != nil {
			return nil, err
		}
		result[path] = data
	}
	return result, nil
}

func validSharedDocument(path string, data []byte, trusted map[string]string) bool {
	if strings.HasPrefix(path, membersPrefix) {
		var record Record
		if json.Unmarshal(data, &record) != nil || VerifySelf(record) != nil || path != membersPrefix+record.Payload.PeerID+".json" {
			return false
		}
		return trusted[record.Payload.PeerID] == "" || trusted[record.Payload.PeerID] == record.Payload.SigningPublicKey
	}
	if strings.HasPrefix(path, revocationsPrefix) {
		var revocation Revocation
		return json.Unmarshal(data, &revocation) == nil && validateRevocation(revocation) == nil && path == revocationsPrefix+revocation.PeerID+".json"
	}
	return false
}

func validPinDocument(path string, data []byte, trusted map[string]string) bool {
	var policy PinPolicy
	if json.Unmarshal(data, &policy) != nil || path != pinPolicyPath(policy.Path) || trusted[policy.IssuedBy] == "" {
		return false
	}
	return verifyPinPolicy(policy, trusted[policy.IssuedBy]) == nil
}

// RefUpdate describes one proposed managed-transport control-ref update.
type RefUpdate struct {
	Old string
	New string
}

// ValidateControlUpdates validates the complete proposed control-plane state
// before receive-pack updates any refs. It accepts legitimate generation
// updates while rejecting deletion, rollback, invalid signatures, and records
// whose authorization chain does not reach a locally pinned administrator.
func ValidateControlUpdates(repositoryPath string, updates map[string]RefUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	membershipRef := SharedRef
	previousMembershipRef := SharedRef
	if update, ok := updates[SharedRef]; ok {
		membershipRef = update.New
		previousMembershipRef = update.Old
	}
	authorized, revoked, err := authorizedRecordsAtRef(repositoryPath, membershipRef, previousMembershipRef)
	if err != nil {
		return fmt.Errorf("validate proposed membership: %w", err)
	}
	for ref, update := range updates {
		if ref != SharedRef && ref != PinRef && ref != ConfigRef {
			continue
		}
		if err := validateDocumentContinuity(repositoryPath, ref, update.Old, update.New); err != nil {
			return err
		}
		switch ref {
		case SharedRef:
			// authorizedRecordsAtRef validated every proposed document.
		case PinRef:
			files, err := loadSharedPrefix(repositoryPath, update.New, "")
			if err != nil {
				return err
			}
			for path, data := range files {
				var policy PinPolicy
				if json.Unmarshal(data, &policy) != nil || path != pinPolicyPath(policy.Path) || revoked[policy.IssuedBy] {
					return fmt.Errorf("invalid cluster pin document %s", path)
				}
				record, ok := authorized[policy.IssuedBy]
				if !ok || policy.FileSystemID != record.Payload.FileSystemID || verifyPinPolicy(policy, record.Payload.SigningPublicKey) != nil {
					return fmt.Errorf("cluster pin document %s was not signed by an accepted member", path)
				}
			}
		case ConfigRef:
			files, err := loadSharedPrefix(repositoryPath, update.New, "")
			if err != nil {
				return err
			}
			for path, data := range files {
				if path != filesystemConfigPath {
					return fmt.Errorf("unexpected filesystem configuration document %s", path)
				}
				var value FilesystemConfig
				if json.Unmarshal(data, &value) != nil {
					return errors.New("invalid filesystem configuration document")
				}
				record, ok := authorized[value.IssuedBy]
				if !ok || record.Payload.Role != "admin" || value.FileSystemID != record.Payload.FileSystemID || revoked[value.IssuedBy] ||
					verifyFilesystemConfig(value, record.Payload.SigningPublicKey) != nil {
					return errors.New("filesystem configuration was not signed by an accepted administrator")
				}
			}
		}
	}
	return nil
}

func authorizedRecordsAtRef(repositoryPath, ref, previousRef string) (map[string]Record, map[string]bool, error) {
	files, err := loadSharedPrefix(repositoryPath, ref, "")
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]Record)
	filesystemID := ""
	for path, data := range files {
		if strings.HasPrefix(path, revocationsPrefix) {
			continue
		}
		if !strings.HasPrefix(path, membersPrefix) {
			return nil, nil, fmt.Errorf("unexpected membership document %s", path)
		}
		var record Record
		if json.Unmarshal(data, &record) != nil || VerifySelf(record) != nil || path != membersPrefix+record.Payload.PeerID+".json" {
			return nil, nil, fmt.Errorf("invalid membership document %s", path)
		}
		if filesystemID != "" && filesystemID != record.Payload.FileSystemID {
			return nil, nil, errors.New("membership ref contains multiple filesystem identities")
		}
		filesystemID = record.Payload.FileSystemID
		byID[record.Payload.PeerID] = record
	}
	bootstrap, err := loadBootstrapAdmins(repositoryPath)
	if err != nil {
		return nil, nil, err
	}
	persisted, err := loadRevoked(repositoryPath)
	if err != nil {
		return nil, nil, err
	}
	preRevocation := authorizeRecords(byID, bootstrap, persisted)
	revoked := make(map[string]bool, len(persisted))
	for id := range persisted {
		revoked[id] = true
	}
	var proposed []Revocation
	previousFiles := make(map[string][]byte)
	if previousRef != "" && strings.Trim(previousRef, "0") != "" {
		previousFiles, err = loadSharedPrefix(repositoryPath, previousRef, revocationsPrefix)
		if err != nil {
			return nil, nil, err
		}
	}
	for path, data := range files {
		if !strings.HasPrefix(path, revocationsPrefix) {
			continue
		}
		var value Revocation
		if json.Unmarshal(data, &value) != nil || path != revocationsPrefix+value.PeerID+".json" {
			return nil, nil, fmt.Errorf("invalid revocation document %s", path)
		}
		if bytes.Equal(previousFiles[path], data) {
			revoked[value.PeerID] = true
			continue
		}
		approver, ok := byID[value.RevokedBy]
		if !ok || preRevocation[value.RevokedBy] != approver.Payload.SigningPublicKey ||
			approver.Payload.Role != "admin" || VerifyRevocation(value, approver) != nil {
			return nil, nil, fmt.Errorf("revocation of %s was not signed by an accepted administrator", value.PeerID)
		}
		proposed = append(proposed, value)
		revoked[value.PeerID] = true
	}
	for _, value := range proposed {
		if revoked[value.RevokedBy] {
			return nil, nil, fmt.Errorf("revoked administrator %s cannot issue revocations", value.RevokedBy)
		}
	}
	authorizedKeys := authorizeRecords(byID, bootstrap, revoked)
	authorized := make(map[string]Record, len(authorizedKeys))
	for id, record := range byID {
		if revoked[id] {
			continue
		}
		if authorizedKeys[id] != record.Payload.SigningPublicKey {
			return nil, nil, fmt.Errorf("membership record %s has no accepted administrator approval", id)
		}
		authorized[id] = record
	}
	return authorized, revoked, nil
}

func validateDocumentContinuity(repositoryPath, ref, old, new string) error {
	if strings.TrimSpace(new) == "" || strings.Trim(new, "0") == "" {
		return fmt.Errorf("DFS control ref %s cannot be deleted", ref)
	}
	if strings.TrimSpace(old) == "" || strings.Trim(old, "0") == "" {
		return nil
	}
	before, err := loadSharedPrefix(repositoryPath, old, "")
	if err != nil {
		return err
	}
	after, err := loadSharedPrefix(repositoryPath, new, "")
	if err != nil {
		return err
	}
	for path, previous := range before {
		candidate, ok := after[path]
		if !ok {
			return fmt.Errorf("DFS control document %s cannot be deleted", path)
		}
		if !bytes.Equal(previous, candidate) && !controlDocumentAdvances(ref, path, candidate, previous) {
			return fmt.Errorf("DFS control document %s did not advance its generation", path)
		}
	}
	return nil
}

func controlDocumentAdvances(ref, path string, candidate, previous []byte) bool {
	if ref != ConfigRef {
		return newerSharedDocument(path, candidate, previous)
	}
	var next, current FilesystemConfig
	if json.Unmarshal(candidate, &next) != nil || json.Unmarshal(previous, &current) != nil {
		return false
	}
	return next.Generation > current.Generation ||
		(next.Generation == current.Generation && next.UpdatedAt.After(current.UpdatedAt))
}

func newerSharedDocument(path string, candidate, current []byte) bool {
	var candidateGeneration, currentGeneration uint64
	var candidateTime, currentTime time.Time
	if strings.HasPrefix(path, membersPrefix) {
		var left, right Record
		if json.Unmarshal(candidate, &left) == nil && json.Unmarshal(current, &right) == nil {
			candidateGeneration, currentGeneration = left.Payload.Generation, right.Payload.Generation
			candidateTime, currentTime = left.Payload.UpdatedAt, right.Payload.UpdatedAt
		}
	} else if strings.HasPrefix(path, revocationsPrefix) {
		var left, right Revocation
		if json.Unmarshal(candidate, &left) == nil && json.Unmarshal(current, &right) == nil {
			candidateGeneration, currentGeneration = left.Generation, right.Generation
			candidateTime, currentTime = left.UpdatedAt, right.UpdatedAt
		}
	} else if strings.HasPrefix(path, pinsPrefix) {
		var left, right PinPolicy
		if json.Unmarshal(candidate, &left) == nil && json.Unmarshal(current, &right) == nil {
			candidateGeneration, currentGeneration = left.Generation, right.Generation
			candidateTime, currentTime = left.UpdatedAt, right.UpdatedAt
		}
	}
	if candidateGeneration != currentGeneration {
		return candidateGeneration > currentGeneration
	}
	if !candidateTime.Equal(currentTime) {
		return candidateTime.After(currentTime)
	}
	return bytes.Compare(candidate, current) > 0
}

func joinSharedHistories(ctx context.Context, repositoryPath string, refs []string) error {
	return joinRefHistories(ctx, repositoryPath, SharedRef, refs, "membership")
}

func joinRefHistories(ctx context.Context, repositoryPath, targetRef string, refs []string, kind string) error {
	current, err := resolveRef(ctx, repositoryPath, targetRef)
	if err != nil || current == "" {
		return err
	}
	var parents []string
	for _, ref := range refs {
		value, _ := resolveRef(ctx, repositoryPath, ref)
		if value == "" || value == current {
			continue
		}
		if _, err := runGit(ctx, repositoryPath, nil, "merge-base", "--is-ancestor", value, current); err != nil {
			parents = append(parents, value)
		}
	}
	if len(parents) == 0 {
		return nil
	}
	tree, err := runGit(ctx, repositoryPath, nil, "rev-parse", current+"^{tree}")
	if err != nil {
		return err
	}
	arguments := []string{"commit-tree", strings.TrimSpace(string(tree)), "-p", current}
	for _, parent := range parents {
		arguments = append(arguments, "-p", parent)
	}
	arguments = append(arguments, "-m", "Merge DFS "+kind+" metadata")
	commit, err := runGit(ctx, repositoryPath, nil, arguments...)
	if err != nil {
		return err
	}
	_, err = runGit(ctx, repositoryPath, nil, "update-ref", targetRef, strings.TrimSpace(string(commit)), current)
	return err
}

func resolveRef(ctx context.Context, repositoryPath, ref string) (string, error) {
	value, err := runGit(ctx, repositoryPath, nil, "rev-parse", "--verify", ref)
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(value)), nil
}

func runGit(ctx context.Context, repositoryPath string, input []byte, arguments ...string) ([]byte, error) {
	return runGitEnv(ctx, repositoryPath, nil, input, arguments...)
}

func runGitEnv(ctx context.Context, repositoryPath string, environment []string, input []byte, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repositoryPath}, arguments...)...)
	command.Env = append(cleanGitRepositoryEnvironment(os.Environ()), environment...)
	command.Env = append(command.Env,
		"GIT_AUTHOR_NAME=DFS", "GIT_AUTHOR_EMAIL=dfs@localhost",
		"GIT_COMMITTER_NAME=DFS", "GIT_COMMITTER_EMAIL=dfs@localhost",
	)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("git %s: %s", strings.Join(arguments, " "), strings.TrimSpace(string(output)))
	}
	return output, nil
}

func cleanGitRepositoryEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		name := value
		if index := strings.IndexByte(name, '='); index >= 0 {
			name = name[:index]
		}
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_PREFIX":
			continue
		default:
			result = append(result, value)
		}
	}
	return result
}
