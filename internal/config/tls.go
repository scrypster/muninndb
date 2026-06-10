package config

// TLSConfig holds TLS settings for inter-node cluster communication.
type TLSConfig struct {
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	CAFile     string `yaml:"ca_file" json:"ca_file"`           // path to CA cert (auto-generated if empty)
	CertFile   string `yaml:"cert_file" json:"cert_file"`       // path to node cert (auto-generated if empty)
	KeyFile    string `yaml:"key_file" json:"key_file"`         // path to node key (auto-generated if empty)
	AutoGenDir string `yaml:"auto_gen_dir" json:"auto_gen_dir"` // directory for auto-generated certs
}
