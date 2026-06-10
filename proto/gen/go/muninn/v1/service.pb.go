// Code generated manually for MuninnDB. DO NOT EDIT.
// This file provides proto message definitions for the MuninnDB service.

package muninn_v1

// HelloRequest message
type HelloRequest struct {
	Version      string   `protobuf:"bytes,1,opt,name=version"`
	AuthMethod   string   `protobuf:"bytes,2,opt,name=auth_method"`
	Token        string   `protobuf:"bytes,3,opt,name=token"`
	Vault        string   `protobuf:"bytes,4,opt,name=vault"`
	Client       string   `protobuf:"bytes,5,opt,name=client"`
	Capabilities []string `protobuf:"bytes,6,rep,name=capabilities"`
}

// HelloResponse message
type HelloResponse struct {
	ServerVersion string   `protobuf:"bytes,1,opt,name=server_version"`
	SessionID     string   `protobuf:"bytes,2,opt,name=session_id"`
	VaultID       string   `protobuf:"bytes,3,opt,name=vault_id"`
	Capabilities  []string `protobuf:"bytes,4,rep,name=capabilities"`
	Limits        *Limits  `protobuf:"bytes,5,opt,name=limits"`
}

// Limits message
type Limits struct {
	MaxResults   int32 `protobuf:"varint,1,opt,name=max_results"`
	MaxHopDepth  int32 `protobuf:"varint,2,opt,name=max_hop_depth"`
	MaxRate      int32 `protobuf:"varint,3,opt,name=max_rate"`
	MaxPayloadMb int32 `protobuf:"varint,4,opt,name=max_payload_mb"`
}

// WriteRequest message
type WriteRequest struct {
	Concept      string        `protobuf:"bytes,1,opt,name=concept"`
	Content      string        `protobuf:"bytes,2,opt,name=content"`
	Tags         []string      `protobuf:"bytes,3,rep,name=tags"`
	Confidence   float32       `protobuf:"fixed32,4,opt,name=confidence"`
	Stability    float32       `protobuf:"fixed32,5,opt,name=stability"`
	Vault        string        `protobuf:"bytes,6,opt,name=vault"`
	IdempotentID string        `protobuf:"bytes,7,opt,name=idempotent_id"`
	Associations []Association `protobuf:"bytes,8,rep,name=associations"`
	Embedding    []float32     `protobuf:"fixed32,9,rep,name=embedding"`
	MemoryType   uint32        `protobuf:"varint,10,opt,name=memory_type"`
	TypeLabel    string        `protobuf:"bytes,11,opt,name=type_label"`
}

// WriteResponse message
type WriteResponse struct {
	ID        string `protobuf:"bytes,1,opt,name=id"`
	CreatedAt int64  `protobuf:"varint,2,opt,name=created_at"`
}

// BatchWriteRequest wraps multiple WriteRequests into a single RPC.
type BatchWriteRequest struct {
	Requests []*WriteRequest `protobuf:"bytes,1,rep,name=requests"`
}

func (m *BatchWriteRequest) GetVault() string {
	if len(m.Requests) > 0 {
		return m.Requests[0].Vault
	}
	return ""
}

// BatchWriteItemResult is the per-item result in a BatchWriteResponse.
type BatchWriteItemResult struct {
	Index int32  `protobuf:"varint,1,opt,name=index"`
	Id    string `protobuf:"bytes,2,opt,name=id"`
	Error string `protobuf:"bytes,3,opt,name=error"`
}

// BatchWriteResponse returns per-item results for a batch write.
type BatchWriteResponse struct {
	Results []*BatchWriteItemResult `protobuf:"bytes,1,rep,name=results"`
}

// ReadRequest message
type ReadRequest struct {
	ID    string `protobuf:"bytes,1,opt,name=id"`
	Vault string `protobuf:"bytes,2,opt,name=vault"`
}

// ReadResponse message
type ReadResponse struct {
	ID          string   `protobuf:"bytes,1,opt,name=id"`
	Concept     string   `protobuf:"bytes,2,opt,name=concept"`
	Content     string   `protobuf:"bytes,3,opt,name=content"`
	Confidence  float32  `protobuf:"fixed32,4,opt,name=confidence"`
	Relevance   float32  `protobuf:"fixed32,5,opt,name=relevance"`
	Tags        []string `protobuf:"bytes,6,rep,name=tags"`
	State       uint32   `protobuf:"varint,7,opt,name=state"`
	CreatedAt   int64    `protobuf:"varint,8,opt,name=created_at"`
	UpdatedAt   int64    `protobuf:"varint,9,opt,name=updated_at"`
	LastAccess  int64    `protobuf:"varint,10,opt,name=last_access"`
	AccessCount uint32   `protobuf:"varint,11,opt,name=access_count"`
	Stability   float32  `protobuf:"fixed32,12,opt,name=stability"`
	MemoryType  uint32   `protobuf:"varint,13,opt,name=memory_type"`
	TypeLabel   string   `protobuf:"bytes,14,opt,name=type_label"`
}

// ForgetRequest message
type ForgetRequest struct {
	ID    string `protobuf:"bytes,1,opt,name=id"`
	Hard  bool   `protobuf:"varint,2,opt,name=hard"`
	Vault string `protobuf:"bytes,3,opt,name=vault"`
}

// ForgetResponse message
type ForgetResponse struct {
	OK bool `protobuf:"varint,1,opt,name=ok"`
}

// StatRequest message
type StatRequest struct {
	Vault string `protobuf:"bytes,1,opt,name=vault"`
}

// StatResponse message
type StatResponse struct {
	EngramCount  int64 `protobuf:"varint,1,opt,name=engram_count"`
	StorageBytes int64 `protobuf:"varint,2,opt,name=storage_bytes"`
	VaultCount   int32 `protobuf:"varint,3,opt,name=vault_count"`
	IndexSize    int64 `protobuf:"varint,4,opt,name=index_size"`
}

// LinkRequest message
type LinkRequest struct {
	SourceID string  `protobuf:"bytes,1,opt,name=source_id"`
	TargetID string  `protobuf:"bytes,2,opt,name=target_id"`
	RelType  uint32  `protobuf:"varint,3,opt,name=rel_type"`
	Weight   float32 `protobuf:"fixed32,4,opt,name=weight"`
	Vault    string  `protobuf:"bytes,5,opt,name=vault"`
}

// LinkResponse message
type LinkResponse struct {
	OK bool `protobuf:"varint,1,opt,name=ok"`
}

// Association message
type Association struct {
	TargetID      string  `protobuf:"bytes,1,opt,name=target_id"`
	RelType       uint32  `protobuf:"varint,2,opt,name=rel_type"`
	Weight        float32 `protobuf:"fixed32,3,opt,name=weight"`
	Confidence    float32 `protobuf:"fixed32,4,opt,name=confidence"`
	CreatedAt     int64   `protobuf:"varint,5,opt,name=created_at"`
	LastActivated int32   `protobuf:"varint,6,opt,name=last_activated"`
}

// ActivateRequest message
type ActivateRequest struct {
	Context    []string  `protobuf:"bytes,1,rep,name=context"`
	Threshold  float32   `protobuf:"fixed32,2,opt,name=threshold"`
	MaxResults int32     `protobuf:"varint,3,opt,name=max_results"`
	MaxHops    int32     `protobuf:"varint,4,opt,name=max_hops"`
	IncludeWhy bool      `protobuf:"varint,5,opt,name=include_why"`
	Vault      string    `protobuf:"bytes,6,opt,name=vault"`
	Weights    *Weights  `protobuf:"bytes,7,opt,name=weights"`
	Filters    []Filter  `protobuf:"bytes,8,rep,name=filters"`
	Embedding  []float32 `protobuf:"fixed32,9,rep,name=embedding"`
}

// Weights message
type Weights struct {
	SemanticSimilarity float32 `protobuf:"fixed32,1,opt,name=semantic_similarity"`
	FullTextRelevance  float32 `protobuf:"fixed32,2,opt,name=full_text_relevance"`
	DecayFactor        float32 `protobuf:"fixed32,3,opt,name=decay_factor"`
	HebbianBoost       float32 `protobuf:"fixed32,4,opt,name=hebbian_boost"`
	AccessFrequency    float32 `protobuf:"fixed32,5,opt,name=access_frequency"`
	Recency            float32 `protobuf:"fixed32,6,opt,name=recency"`
}

// Filter message
type Filter struct {
	Field string `protobuf:"bytes,1,opt,name=field"`
	Op    string `protobuf:"bytes,2,opt,name=op"`
	Value []byte `protobuf:"bytes,3,opt,name=value"`
}

// ActivateResponse message
type ActivateResponse struct {
	QueryID     string           `protobuf:"bytes,1,opt,name=query_id"`
	TotalFound  int32            `protobuf:"varint,2,opt,name=total_found"`
	Activations []ActivationItem `protobuf:"bytes,3,rep,name=activations"`
	LatencyMs   float64          `protobuf:"fixed64,4,opt,name=latency_ms"`
	Frame       int32            `protobuf:"varint,5,opt,name=frame"`
	TotalFrames int32            `protobuf:"varint,6,opt,name=total_frames"`
}

// ActivationItem message
type ActivationItem struct {
	ID              string           `protobuf:"bytes,1,opt,name=id"`
	Concept         string           `protobuf:"bytes,2,opt,name=concept"`
	Content         string           `protobuf:"bytes,3,opt,name=content"`
	Score           float32          `protobuf:"fixed32,4,opt,name=score"`
	Confidence      float32          `protobuf:"fixed32,5,opt,name=confidence"`
	ScoreComponents *ScoreComponents `protobuf:"bytes,6,opt,name=score_components"`
	Why             string           `protobuf:"bytes,7,opt,name=why"`
	HopPath         []string         `protobuf:"bytes,8,rep,name=hop_path"`
	Dormant         bool             `protobuf:"varint,9,opt,name=dormant"`
}

// ScoreComponents message
type ScoreComponents struct {
	SemanticSimilarity float32 `protobuf:"fixed32,1,opt,name=semantic_similarity"`
	FullTextRelevance  float32 `protobuf:"fixed32,2,opt,name=full_text_relevance"`
	DecayFactor        float32 `protobuf:"fixed32,3,opt,name=decay_factor"`
	HebbianBoost       float32 `protobuf:"fixed32,4,opt,name=hebbian_boost"`
	AccessFrequency    float32 `protobuf:"fixed32,5,opt,name=access_frequency"`
	Recency            float32 `protobuf:"fixed32,6,opt,name=recency"`
	Raw                float32 `protobuf:"fixed32,7,opt,name=raw"`
	Final              float32 `protobuf:"fixed32,8,opt,name=final"`
}

// SubscribeRequest message
type SubscribeRequest struct {
	SubscriptionID string   `protobuf:"bytes,1,opt,name=subscription_id"`
	Context        []string `protobuf:"bytes,2,rep,name=context"`
	Threshold      float32  `protobuf:"fixed32,3,opt,name=threshold"`
	Vault          string   `protobuf:"bytes,4,opt,name=vault"`
	TTL            int32    `protobuf:"varint,5,opt,name=ttl"`
	RateLimit      int32    `protobuf:"varint,6,opt,name=rate_limit"`
	PushOnWrite    bool     `protobuf:"varint,7,opt,name=push_on_write"`
	DeltaThreshold float32  `protobuf:"fixed32,8,opt,name=delta_threshold"`
}

// SubscribeResponse message
type SubscribeResponse struct {
	SubID  string `protobuf:"bytes,1,opt,name=sub_id"`
	Status string `protobuf:"bytes,2,opt,name=status"`
}

// ActivationPush message
type ActivationPush struct {
	SubscriptionID string          `protobuf:"bytes,1,opt,name=subscription_id"`
	Activation     *ActivationItem `protobuf:"bytes,2,opt,name=activation"`
	Trigger        string          `protobuf:"bytes,3,opt,name=trigger"`
	PushNumber     int32           `protobuf:"varint,4,opt,name=push_number"`
	At             int64           `protobuf:"varint,5,opt,name=at"`
}
