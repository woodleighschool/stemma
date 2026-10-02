package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// Targets are release identities, never properties of the machine running Stemma.
var brewReleases = []struct{ version, name string }{
	{"27", "golden_gate"}, {"26", "tahoe"}, {"15", "sequoia"}, {"14", "sonoma"},
	{"13", "ventura"}, {"12", "monterey"}, {"11", "big_sur"},
}

type homebrewConfig struct {
	Cask         string `json:"cask,omitempty" jsonschema_description:"Homebrew cask token. Selects its vendor download; no cask actions run."`
	Formula      string `json:"formula,omitempty" jsonschema_description:"Homebrew core formula token. Selects a standalone bottle requiring no relocation, runtime formula dependencies or post-install actions. No source-build fallback."`
	Architecture string `json:"architecture,omitempty" jsonschema:"enum=arm64,enum=x86_64" jsonschema_description:"Target architecture. Defaults to arm64, independently of the runner."`
	MacOS        string `json:"macos,omitempty" jsonschema_description:"Target macOS major version. Omit for the current supported release (27). Update locks the concrete target and selected variant."`
	Language     string `json:"language,omitempty" jsonschema_description:"Cask language alias. Omit for the cask's declared default language."`
}

var brewToken = regexp.MustCompile(`^[a-z0-9][a-z0-9@+._-]*$`)

func homebrewInput(input plugin.Input) (homebrewConfig, error) {
	var s homebrewConfig
	data, err := json.Marshal(input.Config)
	if err == nil {
		err = decode(data, &s)
	}
	if err != nil {
		return s, fmt.Errorf("homebrew config: %w", err)
	}
	if (s.Cask == "") == (s.Formula == "") {
		return s, errors.New("homebrew requires exactly one of cask or formula")
	}
	token := s.Cask
	if token == "" {
		token = s.Formula
	}
	if !brewToken.MatchString(token) {
		return s, errors.New("homebrew requires a core formula or cask token")
	}
	if s.Architecture == "" {
		s.Architecture = "arm64"
	}
	if s.Architecture != "arm64" && s.Architecture != "x86_64" {
		return s, errors.New("homebrew architecture must be arm64 or x86_64")
	}
	if s.MacOS != "" && !slices.ContainsFunc(brewReleases, func(r struct{ version, name string }) bool { return r.version == s.MacOS }) {
		return s, fmt.Errorf("unsupported macOS target %q", s.MacOS)
	}
	if s.Formula != "" && s.Language != "" {
		return s, errors.New("language only applies to casks")
	}
	return s, nil
}

func (m *Manager) homebrewResolver() Resolver {
	return Resolver{Version: "1", Discover: m.discoverHomebrew, Fingerprint: func(input plugin.Input) (string, error) {
		s, err := homebrewInput(input)
		if err != nil {
			return "", err
		}
		return fingerprint(s)
	}}
}

// homebrewObservation is sufficient for cold replay, including hashless casks.
type homebrewObservation struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	SHA256   string `json:"sha256,omitempty"`
	Bottle   bool   `json:"bottle,omitempty"`
}

type brewDocument struct {
	Name               json.RawMessage `json:"name"`
	Token              string          `json:"token"`
	Version            string          `json:"version"`
	URL                string          `json:"url"`
	SHA256             string          `json:"sha256"`
	Disabled           bool            `json:"disabled"`
	SupportedPlatforms []string        `json:"supported_platforms"`
	URLSpecs           struct {
		UserAgent string            `json:"user_agent"`
		Referer   string            `json:"referer"`
		Cookies   map[string]string `json:"cookies"`
		Using     string            `json:"using"`
	} `json:"url_specs"`
	LanguageVariations []struct {
		Languages []string `json:"languages"`
		Default   bool     `json:"default"`
		Value     string   `json:"value"`
		URL       string   `json:"url"`
		SHA256    string   `json:"sha256"`
	} `json:"language_variations"`
	Artifacts []map[string]json.RawMessage `json:"artifacts"`
	Versions  struct {
		Stable string `json:"stable"`
	} `json:"versions"`
	Revision                int             `json:"revision"`
	Dependencies            []string        `json:"dependencies"`
	RecommendedDependencies []string        `json:"recommended_dependencies"`
	PostInstall             bool            `json:"post_install_defined"`
	PourOnlyIf              json.RawMessage `json:"pour_bottle_only_if"`
	Requirements            []struct {
		Name     string          `json:"name"`
		Version  json.RawMessage `json:"version"`
		Contexts []string        `json:"contexts"`
	} `json:"requirements"`
	UsesFromMacOS       []json.RawMessage   `json:"uses_from_macos"`
	UsesFromMacOSBounds []map[string]string `json:"uses_from_macos_bounds"`
	Bottle              struct {
		Stable struct {
			Rebuild int `json:"rebuild"`
			Files   map[string]struct {
				Cellar string `json:"cellar"`
				URL    string `json:"url"`
				SHA256 string `json:"sha256"`
			} `json:"files"`
		} `json:"stable"`
	} `json:"bottle"`
}

type homebrewEvidence struct {
	Name         string                       `json:"name"`
	Version      string                       `json:"version"`
	Architecture string                       `json:"architecture"`
	MacOS        string                       `json:"macos"`
	Language     string                       `json:"language,omitempty"`
	Artifacts    []map[string]json.RawMessage `json:"artifacts,omitempty"`
	Revision     *int                         `json:"revision,omitempty"`
	BottleTag    string                       `json:"bottle_tag,omitempty"`
	Rebuild      *int                         `json:"rebuild,omitempty"`
	Cellar       string                       `json:"cellar,omitempty"`
	PayloadRoot  string                       `json:"payload_root,omitempty"`
}

func (m *Manager) discoverHomebrew(ctx context.Context, input plugin.Input) (Discovery, error) {
	s, err := homebrewInput(input)
	if err != nil {
		return Discovery{}, err
	}
	if s.MacOS == "" {
		s.MacOS = brewReleases[0].version
	}
	kind, name := "cask", s.Cask
	if s.Formula != "" {
		kind, name = "formula", s.Formula
	}
	data, err := m.metadataBytes(ctx, "https://formulae.brew.sh/api/"+kind+"/"+name+".json")
	if err != nil {
		return Discovery{}, fmt.Errorf("homebrew %s: %w", name, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Discovery{}, fmt.Errorf("homebrew metadata: %w", err)
	}
	tag := brewTag(s)
	var variations map[string]map[string]json.RawMessage
	if err := json.Unmarshal(fields["variations"], &variations); err != nil && fields["variations"] != nil {
		return Discovery{}, err
	}
	maps.Copy(fields, variations[tag])
	data, err = json.Marshal(fields)
	if err != nil {
		return Discovery{}, err
	}
	var doc brewDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return Discovery{}, fmt.Errorf("homebrew metadata: %w", err)
	}
	if doc.Disabled {
		return Discovery{}, errors.New("homebrew entry is disabled")
	}
	evidence := homebrewEvidence{Name: name, Architecture: s.Architecture, MacOS: s.MacOS}
	observed := homebrewObservation{}
	headers := map[string]string{}
	if kind == "cask" {
		if doc.Token != name {
			return Discovery{}, errors.New("homebrew returned a different cask")
		}
		if !slices.Contains(doc.SupportedPlatforms, tag) {
			return Discovery{}, fmt.Errorf("cask %s does not support %s", name, tag)
		}
		evidence.Version = doc.Version
		observed.URL, observed.SHA256 = doc.URL, doc.SHA256
		selected := s.Language == ""
		for _, language := range doc.LanguageVariations {
			if s.Language != "" && slices.Contains(language.Languages, s.Language) || s.Language == "" && language.Default {
				observed.URL, observed.SHA256 = language.URL, language.SHA256
				evidence.Language, selected = language.Value, true
				break
			}
		}
		if !selected {
			return Discovery{}, fmt.Errorf("cask %s has no language %q", name, s.Language)
		}
		if doc.URLSpecs.Using != "" && doc.URLSpecs.Using != "curl" {
			return Discovery{}, fmt.Errorf("unsupported cask download strategy %q", doc.URLSpecs.Using)
		}
		if doc.URLSpecs.UserAgent != "" {
			headers["User-Agent"] = doc.URLSpecs.UserAgent
		}
		if doc.URLSpecs.Referer != "" {
			headers["Referer"] = doc.URLSpecs.Referer
		}
		if len(doc.URLSpecs.Cookies) > 0 {
			req := &http.Request{Header: make(http.Header)}
			for _, key := range slices.Sorted(maps.Keys(doc.URLSpecs.Cookies)) {
				// #nosec G124 -- This is an outbound vendor cookie, not a server session cookie.
				req.AddCookie(&http.Cookie{Name: key, Value: doc.URLSpecs.Cookies[key]})
			}
			headers["Cookie"] = req.Header.Get("Cookie")
		}
		// Only payload declarations are hints. Lifecycle actions are not executable evidence.
		for _, artifact := range doc.Artifacts {
			for key := range artifact {
				if slices.Contains([]string{"app", "pkg", "binary", "suite"}, key) {
					evidence.Artifacts = append(evidence.Artifacts, artifact)
					break
				}
			}
		}
		if observed.SHA256 == "no_check" {
			observed.SHA256 = ""
		}
	} else {
		var formulaName string
		_ = json.Unmarshal(doc.Name, &formulaName)
		if formulaName != name {
			return Discovery{}, errors.New("homebrew returned a different formula")
		}
		if err := standaloneBottle(doc, s); err != nil {
			return Discovery{}, fmt.Errorf("formula %s: %w", name, err)
		}
		// Older bottles run on newer macOS releases; never cross architecture or use a newer bottle.
		var tags []string
		eligible := false
		for _, release := range brewReleases {
			if release.version == s.MacOS {
				eligible = true
			}
			if eligible {
				target := s
				target.MacOS = release.version
				tags = append(tags, brewTag(target))
			}
		}
		tags = append(tags, "all")
		for _, candidate := range tags {
			if _, ok := doc.Bottle.Stable.Files[candidate]; ok {
				evidence.BottleTag = candidate
				break
			}
		}
		if evidence.BottleTag == "" {
			return Discovery{}, fmt.Errorf("formula %s has no compatible bottle for %s", name, tag)
		}
		bottle := doc.Bottle.Stable.Files[evidence.BottleTag]
		if strings.TrimPrefix(bottle.Cellar, ":") != "any_skip_relocation" {
			return Discovery{}, fmt.Errorf("formula %s bottle requires Homebrew relocation or Cellar %q", name, bottle.Cellar)
		}
		evidence.Version, evidence.Revision, evidence.Rebuild = doc.Versions.Stable, &doc.Revision, &doc.Bottle.Stable.Rebuild
		evidence.Cellar = "any_skip_relocation"
		evidence.PayloadRoot = name + "/" + evidence.Version
		if doc.Revision > 0 {
			evidence.PayloadRoot += "_" + strconv.Itoa(doc.Revision)
		}
		observed = homebrewObservation{URL: bottle.URL, SHA256: bottle.SHA256, Bottle: true, Filename: name + "-" + path.Base(evidence.PayloadRoot) + "." + evidence.BottleTag + ".bottle.tar.gz"}
		u, err := url.Parse(observed.URL)
		if err != nil || u.Scheme != "https" || u.Host != "ghcr.io" || u.Path != "/v2/homebrew/core/"+strings.ReplaceAll(name, "@", "/")+"/blobs/sha256:"+observed.SHA256 || u.RawQuery != "" {
			return Discovery{}, errors.New("unsupported Homebrew bottle registry URL")
		}
	}
	if evidence.Version == "" {
		return Discovery{}, errors.New("homebrew metadata has no version")
	}
	if err := validateHTTPURL(observed.URL); err != nil {
		return Discovery{}, err
	}
	if observed.Filename == "" {
		u, _ := url.Parse(observed.URL)
		observed.Filename = path.Base(u.Path)
	}
	if !validFilename(observed.Filename) {
		return Discovery{}, errors.New("homebrew download has no safe filename")
	}
	if observed.SHA256 != "" && !validDigest(observed.SHA256) {
		return Discovery{}, errors.New("homebrew returned an invalid sha256")
	}
	if observed.Bottle && observed.SHA256 == "" {
		return Discovery{}, errors.New("homebrew bottle has no sha256")
	}
	observation, _ := json.Marshal(observed)
	claims, _ := json.Marshal(evidence)
	found := Discovery{Observation: observation, Evidence: map[string]json.RawMessage{"homebrew." + kind: claims}}
	if observed.SHA256 != "" {
		found.Content = &Content{SHA256: observed.SHA256, Filename: observed.Filename, Mode: 0o644}
	}
	if !observed.Bottle {
		found.Download = &plugin.Download{URL: observed.URL, Headers: headers}
	}
	return found, nil
}

func brewTag(s homebrewConfig) string {
	for _, r := range brewReleases {
		if r.version == s.MacOS {
			if s.Architecture == "arm64" {
				return "arm64_" + r.name
			}
			return r.name
		}
	}
	return ""
}

func standaloneBottle(doc brewDocument, s homebrewConfig) error {
	if len(doc.Dependencies) > 0 || len(doc.RecommendedDependencies) > 0 {
		return errors.New("runtime formula dependencies require a Homebrew installation")
	}
	if doc.PostInstall {
		return errors.New("post-install actions are not supported")
	}
	if len(doc.PourOnlyIf) > 0 && string(doc.PourOnlyIf) != "null" {
		return errors.New("conditional bottle installation is not supported")
	}
	for _, requirement := range doc.Requirements {
		if len(requirement.Contexts) > 0 && !slices.Contains(requirement.Contexts, "run") {
			continue
		}
		switch requirement.Name {
		case "macos":
			if len(requirement.Version) > 0 && string(requirement.Version) != "null" {
				var minimum string
				if json.Unmarshal(requirement.Version, &minimum) != nil {
					return errors.New("unsupported macOS requirement")
				}
				target, _ := strconv.Atoi(s.MacOS)
				minimumMajor, _ := strconv.Atoi(strings.Split(minimum, ".")[0])
				if minimumMajor == 0 || target < minimumMajor {
					return fmt.Errorf("requires macOS %s", minimum)
				}
			}
		case "arch":
			var arch string
			_ = json.Unmarshal(requirement.Version, &arch)
			if arch != s.Architecture {
				return fmt.Errorf("requires architecture %s", arch)
			}
		default:
			return fmt.Errorf("unsupported runtime requirement %s", requirement.Name)
		}
	}
	for i, dep := range doc.UsesFromMacOS {
		var contexts map[string]string
		if json.Unmarshal(dep, &contexts) == nil {
			buildOnly := true
			for _, context := range contexts {
				if context != "build" && context != "test" {
					buildOnly = false
				}
			}
			if buildOnly {
				continue
			}
		}
		if i < len(doc.UsesFromMacOSBounds) {
			since := doc.UsesFromMacOSBounds[i]["since"]
			if since != "" {
				minimum := 0
				for _, r := range brewReleases {
					if r.name == since {
						minimum, _ = strconv.Atoi(r.version)
					}
				}
				target, _ := strconv.Atoi(s.MacOS)
				if minimum == 0 || target < minimum {
					return fmt.Errorf("system dependency requires macOS %s", since)
				}
			}
		}
	}
	return nil
}

func (m *Manager) fetchHomebrew(ctx context.Context, observed json.RawMessage, previous *record) (record, bool, error) {
	var selected homebrewObservation
	if err := decode(observed, &selected); err != nil {
		return record{}, false, err
	}
	if !selected.Bottle {
		return record{}, false, errors.New("cask lock is missing download instructions")
	}
	return m.download(ctx, nativeConfig{Type: "homebrew", URL: selected.URL, Filename: selected.Filename, SHA256: selected.SHA256}, selected.URL, previous)
}

func (m *Manager) metadataBytes(ctx context.Context, address string) ([]byte, error) {
	req, err := m.request(ctx, address, nativeConfig{})
	if err != nil {
		return nil, err
	}
	response, err := m.metadataClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata returned HTTP %d", response.StatusCode)
	}
	return io.ReadAll(response.Body)
}
