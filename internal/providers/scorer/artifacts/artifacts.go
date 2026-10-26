// Package artifacts stores learned scorer models in the models bucket and lists the platform's
// own scorers (docs/MODELS.md §2): platform models live under platform/{id}/ as model.onnx and
// features.json (written by tools/training), tenant uploads under tenants/{tid}/models/.
package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Scorer types (tenant.ScorerONNX, tenant.ScorerKServeV2).
const (
	TypeONNX     = "onnx"
	TypeKServeV2 = "kserve_v2"
)

// PlatformScorer is a scorer the platform trains and serves.
type PlatformScorer struct {
	ID   string
	Type string
	// Triton is the model name in the platform's Triton (kserve_v2 scorers).
	Triton string
}

// Platform lists the platform scorers: prerank in-process, rank through Triton's ONNX backend.
var Platform = map[string]PlatformScorer{
	"prerank-gbdt-v1": {ID: "prerank-gbdt-v1", Type: TypeONNX},
	"rank-gbdt-v1":    {ID: "rank-gbdt-v1", Type: TypeKServeV2, Triton: "rank_gbdt_v1"},
}

// ErrNotFound is returned for a missing artifact.
var ErrNotFound = errors.New("artifact not found")

// MaxBytes bounds an artifact read (docs/ARCHITECTURE.md §9: ONNX uploads ≤ 200 MB).
const MaxBytes = 200 << 20

// Manifest is a platform model's features.json: the ordered feature schema the model was
// trained on, and how it was trained.
type Manifest struct {
	ID        string             `json:"id"`
	Type      string             `json:"type"`
	Features  []string           `json:"features"`
	SHA256    string             `json:"sha256"`
	TrainedAt string             `json:"trained_at,omitempty"`
	Dataset   string             `json:"dataset,omitempty"`
	Metrics   map[string]float64 `json:"metrics,omitempty"`
}

// Store reads and writes artifacts.
type Store struct {
	Client *s3.Client
	Bucket string
}

// PlatformModelKey and PlatformManifestKey locate a platform scorer's files.
func PlatformModelKey(id string) string    { return "platform/" + id + "/model.onnx" }
func PlatformManifestKey(id string) string { return "platform/" + id + "/features.json" }

// UploadKey is where a tenant's presigned model upload lands.
func UploadKey(tid, uploadID string) string {
	return "tenants/" + tid + "/models/" + uploadID + ".onnx"
}

// Get reads an object of at most MaxBytes.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.Bucket, Key: aws.String(key)})
	var nk *s3types.NoSuchKey
	if errors.As(err, &nk) {
		return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	if len(b) > MaxBytes {
		return nil, fmt.Errorf("%s exceeds %d MB", key, MaxBytes>>20)
	}
	return b, nil
}

// Size returns an object's size.
func (s *Store) Size(ctx context.Context, key string) (int64, error) {
	out, err := s.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.Bucket, Key: aws.String(key)})
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return 0, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("head %s: %w", key, err)
	}
	return aws.ToInt64(out.ContentLength), nil
}

// Put writes an object.
func (s *Store) Put(ctx context.Context, key string, data []byte) error {
	if _, err := s.Client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.Bucket, Key: aws.String(key), Body: bytes.NewReader(data)}); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// Manifest reads a platform scorer's features.json.
func (s *Store) Manifest(ctx context.Context, id string) (Manifest, error) {
	b, err := s.Get(ctx, PlatformManifestKey(id))
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", PlatformManifestKey(id), err)
	}
	if len(m.Features) == 0 {
		return Manifest{}, fmt.Errorf("%s lists no features", PlatformManifestKey(id))
	}
	return m, nil
}

// PlatformModel reads a platform ONNX scorer and its manifest, checking the recorded SHA-256.
func (s *Store) PlatformModel(ctx context.Context, id string) ([]byte, Manifest, error) {
	m, err := s.Manifest(ctx, id)
	if err != nil {
		return nil, m, err
	}
	b, err := s.Get(ctx, PlatformModelKey(id))
	if err != nil {
		return nil, m, err
	}
	if m.SHA256 != "" && SHA256(b) != m.SHA256 {
		return nil, m, fmt.Errorf("%s does not match its manifest's sha256", PlatformModelKey(id))
	}
	return b, m, nil
}

// SHA256 is the hex digest of b.
func SHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
