package inventory

import (
	"bufio"
	"context"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

// StaticSource reads hosts from a YAML file or a plain text list.
type StaticSource struct {
	Path string
	// Now stamps SeenAt; nil means time.Now.
	Now func() time.Time
}

// NewStaticSource returns a source reading path.
func NewStaticSource(path string) *StaticSource { return &StaticSource{Path: path} }

// Name is both the source name and its label namespace.
func (s *StaticSource) Name() string { return "static" }

type staticFile struct {
	Hosts []staticHost `yaml:"hosts"`
}

type staticHost struct {
	Name    string            `yaml:"name"`
	Addr    string            `yaml:"addr"`
	Aliases []string          `yaml:"aliases"`
	Labels  map[string]string `yaml:"labels"`
}

// Load reads the file named by Path. Files ending in .yaml/.yml are parsed as
// YAML; anything else is one host per line with "#" comments.
func (s *StaticSource) Load(ctx context.Context) ([]Host, error) {
	if s.Path == "" {
		return nil, errs.New(errs.Config, "inventory.static", "no path")
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, errs.Wrap(errs.Config, "inventory.static", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, errs.Wrap(errs.Canceled, "inventory.static", err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	stamp := now()
	if isYAMLPath(s.Path) {
		return parseStaticYAML(data, stamp)
	}
	return parseStaticPlain(data, stamp), nil
}

func isYAMLPath(p string) bool {
	lower := strings.ToLower(p)
	return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")
}

func parseStaticYAML(data []byte, stamp time.Time) ([]Host, error) {
	var f staticFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, errs.Wrap(errs.Parse, "inventory.static", err)
	}
	hosts := make([]Host, 0, len(f.Hosts))
	for _, sh := range f.Hosts {
		id := resource.NormalizeHostID(sh.Name)
		if id == "" {
			continue
		}
		h := Host{
			ID:      id,
			Addr:    sh.Addr,
			Aliases: append([]string(nil), sh.Aliases...),
			Labels:  make(map[string]string, len(sh.Labels)),
			SeenAt:  stamp,
		}
		for k, v := range sh.Labels {
			h.Labels[k] = v
		}
		hosts = append(hosts, h)
	}
	return hosts, nil
}

func parseStaticPlain(data []byte, stamp time.Time) []Host {
	var hosts []Host
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id := resource.NormalizeHostID(line)
		if id == "" {
			continue
		}
		hosts = append(hosts, Host{ID: id, Addr: line, SeenAt: stamp})
	}
	return hosts
}
