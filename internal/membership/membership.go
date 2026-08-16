package membership

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
)

const (
	Version             = 1
	sharedDirectory     = ".dfs/members"
	privateKeyFile      = "membership-key.pem"
	trustedMembersFile  = "trusted-members.json"
	revokedMembersFile  = "revoked-members.json"
	membershipAttribute = ".dfs/members/** annex.largefiles=nothing"
	revocationAttribute = ".dfs/revocations/** annex.largefiles=nothing"
)

func KeyPath(repositoryPath string) string {
	return filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), privateKeyFile)
}

type SSHTransport struct {
	Endpoint  string   `json:"endpoint"`
	PublicKey string   `json:"public_key"`
	HostKeys  []string `json:"host_keys"`
}

type Payload struct {
	Version          int          `json:"version"`
	FileSystemID     string       `json:"filesystem_id"`
	PeerID           string       `json:"peer_id"`
	Name             string       `json:"name"`
	Hostname         string       `json:"hostname"`
	Role             string       `json:"role"`
	SigningPublicKey string       `json:"signing_public_key"`
	SSH              SSHTransport `json:"ssh"`
	Generation       uint64       `json:"generation"`
	UpdatedAt        time.Time    `json:"updated_at"`
	Revoked          bool         `json:"revoked"`
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
	if err := ensureAttributes(repositoryPath); err != nil {
		return err
	}
	directory := filepath.Join(repositoryPath, ".dfs", "revocations")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(revocation, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(directory, revocation.PeerID+".json")
	temporary := path + ".new"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
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
	if err := ensureAttributes(repositoryPath); err != nil {
		return err
	}
	directory := filepath.Join(repositoryPath, filepath.FromSlash(sharedDirectory))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(directory, record.Payload.PeerID+".json")
	temporary := path + ".new"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func LoadAll(repositoryPath string) ([]Record, error) {
	directory := filepath.Join(repositoryPath, filepath.FromSlash(sharedDirectory))
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("decode membership %s: %w", entry.Name(), err)
		}
		if record.Payload.PeerID+".json" != entry.Name() {
			return nil, fmt.Errorf("membership filename does not match peer ID: %s", entry.Name())
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
	if existing := trusted[peerID]; existing != "" && signingPublicKey != "" && existing != signingPublicKey {
		return errors.New("membership signing key does not match the trusted key")
	}
	if trusted[peerID] == "" || signingPublicKey != "" {
		trusted[peerID] = signingPublicKey
	}
	data, err := json.MarshalIndent(trusted, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), trustedMembersFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".new"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
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

// Accepted returns the non-revoked records whose approval chain reaches a
// locally trusted peer. Newly accepted peers are persisted as trust anchors so
// offline reconciliation does not depend on record ordering.
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
	changed := true
	for id, key := range trusted {
		if key == "" {
			if record, ok := byID[id]; ok {
				trusted[id] = record.Payload.SigningPublicKey
			}
		}
	}
	for changed {
		changed = false
		for id, record := range byID {
			if trusted[id] != "" || trusted[record.ApprovedBy] == "" {
				continue
			}
			approver, ok := byID[record.ApprovedBy]
			if ok && approver.Payload.SigningPublicKey == trusted[record.ApprovedBy] && VerifyApproval(record, approver) == nil {
				trusted[id] = record.Payload.SigningPublicKey
				changed = true
			}
		}
	}
	revoked, err := acceptedRevocations(repositoryPath, filesystemID, byID, trusted)
	if err != nil {
		return nil, err
	}
	persistedRevoked, err := loadRevoked(repositoryPath)
	if err != nil {
		return nil, err
	}
	for id := range revoked {
		persistedRevoked[id] = true
	}
	if err := saveRevoked(repositoryPath, persistedRevoked); err != nil {
		return nil, err
	}
	revoked = persistedRevoked
	var accepted []Record
	for id, key := range trusted {
		if record, ok := byID[id]; ok && record.Payload.SigningPublicKey == key && !record.Payload.Revoked && !revoked[id] {
			accepted = append(accepted, record)
		}
	}
	for id, key := range trusted {
		if key != "" {
			if err := Trust(repositoryPath, id, key); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(accepted, func(i, j int) bool { return accepted[i].Payload.PeerID < accepted[j].Payload.PeerID })
	return accepted, nil
}

func AcceptedRevocations(repositoryPath, filesystemID string) (map[string]bool, error) {
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
		byID[record.Payload.PeerID] = record
	}
	revoked, err := acceptedRevocations(repositoryPath, filesystemID, byID, trusted)
	if err != nil {
		return nil, err
	}
	persisted, err := loadRevoked(repositoryPath)
	if err != nil {
		return nil, err
	}
	for id := range revoked {
		persisted[id] = true
	}
	if err := saveRevoked(repositoryPath, persisted); err != nil {
		return nil, err
	}
	return persisted, nil
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

func validateRevocation(revocation Revocation) error {
	if revocation.Version != Version || len(revocation.FileSystemID) < 16 || !validPeerID(revocation.PeerID) ||
		!validPeerID(revocation.RevokedBy) || revocation.Generation == 0 || revocation.UpdatedAt.IsZero() {
		return errors.New("incomplete membership revocation")
	}
	return nil
}

func acceptedRevocations(repositoryPath, filesystemID string, records map[string]Record, trusted map[string]string) (map[string]bool, error) {
	result := make(map[string]bool)
	directory := filepath.Join(repositoryPath, ".dfs", "revocations")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var revocation Revocation
		if json.Unmarshal(data, &revocation) != nil || revocation.FileSystemID != filesystemID || revocation.PeerID+".json" != entry.Name() {
			continue
		}
		approver, ok := records[revocation.RevokedBy]
		if ok && approver.Payload.SigningPublicKey == trusted[revocation.RevokedBy] && VerifyRevocation(revocation, approver) == nil {
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
	temporary := path + ".new"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
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
	if strings.TrimSpace(payload.SSH.Endpoint) == "" || strings.TrimSpace(payload.SSH.PublicKey) == "" {
		return errors.New("membership SSH transport is incomplete")
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

func ensureAttributes(repositoryPath string) error {
	path := filepath.Join(repositoryPath, ".gitattributes")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	hasMembership, hasRevocations := false, false
	for _, line := range strings.Split(string(data), "\n") {
		hasMembership = hasMembership || strings.TrimSpace(line) == membershipAttribute
		hasRevocations = hasRevocations || strings.TrimSpace(line) == revocationAttribute
	}
	if hasMembership && hasRevocations {
		return nil
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	if !hasMembership {
		data = append(data, membershipAttribute...)
		data = append(data, '\n')
	}
	if !hasRevocations {
		data = append(data, revocationAttribute...)
		data = append(data, '\n')
	}
	return os.WriteFile(path, data, 0o644)
}
