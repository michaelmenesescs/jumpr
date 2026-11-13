package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestLoadElasticsearchSection checks the `elasticsearch:` section is decoded
// with its log sources (SPEC §8.5).
func TestLoadElasticsearchSection(t *testing.T) {
	body := "elasticsearch:\n" +
		"  - name: eu\n" +
		"    url: https://elastic.example.internal:9200\n" +
		"    connection: eu-sso\n" +
		"    sources:\n" +
		"      - name: app\n" +
		"        index: logs-app-*\n" +
		"        time_field: '@timestamp'\n" +
		"        host_field: host.name\n" +
		"        message_field: message\n" +
		"        level_field: log.level\n" +
		"      - name: system\n" +
		"        index: 'remote-eu:logs-sys-*'\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path, t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []ElasticsearchConfig{{
		Name:       "eu",
		URL:        "https://elastic.example.internal:9200",
		Connection: "eu-sso",
		Sources: []LogSourceConfig{
			{
				Name:         "app",
				Index:        "logs-app-*",
				TimeField:    "@timestamp",
				HostField:    "host.name",
				MessageField: "message",
				LevelField:   "log.level",
			},
			{Name: "system", Index: "remote-eu:logs-sys-*"},
		},
	}}
	if !reflect.DeepEqual(cfg.Elasticsearch, want) {
		t.Errorf("elasticsearch = %+v, want %+v", cfg.Elasticsearch, want)
	}
	if got := Default().Elasticsearch; len(got) != 0 {
		t.Errorf("Default().Elasticsearch = %v, want empty", got)
	}
}

// TestValidateElasticsearchSection checks every rule Validate applies to a
// cluster and its log sources.
func TestValidateElasticsearchSection(t *testing.T) {
	source := LogSourceConfig{Name: "app", Index: "logs-app-*"}
	cases := []struct {
		name string
		set  func(*Config)
		want string // substring of the error; empty means valid
	}{
		{"one cluster", func(c *Config) {
			c.Elasticsearch = []ElasticsearchConfig{{
				Name: "eu", URL: "https://elastic.example.internal:9200",
				Sources: []LogSourceConfig{source},
			}}
		}, ""},
		{"without sources", func(c *Config) {
			c.Elasticsearch = []ElasticsearchConfig{{Name: "eu", URL: "https://elastic.example.internal:9200"}}
		}, ""},
		{"without a name", func(c *Config) {
			c.Elasticsearch = []ElasticsearchConfig{{URL: "https://elastic.example.internal:9200"}}
		}, "elasticsearch[0].name"},
		{"configured twice", func(c *Config) {
			c.Elasticsearch = []ElasticsearchConfig{
				{Name: "eu", URL: "https://elastic.example.internal:9200"},
				{Name: "eu", URL: "https://elastic.example.internal:9200"},
			}
		}, "elasticsearch[1].name"},
		{"without a scheme", func(c *Config) {
			c.Elasticsearch = []ElasticsearchConfig{{Name: "eu", URL: "elastic.example.internal:9200"}}
		}, "elasticsearch[0].url"},
		{"unknown scheme", func(c *Config) {
			c.Elasticsearch = []ElasticsearchConfig{{Name: "eu", URL: "ftp://elastic.example.internal"}}
		}, "elasticsearch[0].url"},
		{"source without a name", func(c *Config) {
			c.Elasticsearch = []ElasticsearchConfig{{
				Name: "eu", URL: "https://elastic.example.internal:9200",
				Sources: []LogSourceConfig{{Index: "logs-app-*"}},
			}}
		}, "elasticsearch[0].sources[0].name"},
		{"source configured twice", func(c *Config) {
			c.Elasticsearch = []ElasticsearchConfig{{
				Name: "eu", URL: "https://elastic.example.internal:9200",
				Sources: []LogSourceConfig{source, source},
			}}
		}, "elasticsearch[0].sources[1].name"},
		{"source without an index", func(c *Config) {
			c.Elasticsearch = []ElasticsearchConfig{{
				Name: "eu", URL: "https://elastic.example.internal:9200",
				Sources: []LogSourceConfig{{Name: "app"}},
			}}
		}, "elasticsearch[0].sources[0].index"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.set(&cfg)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate accepted an invalid elasticsearch section")
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
