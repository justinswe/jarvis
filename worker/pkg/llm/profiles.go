package llm

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/justinswe/std/errors"
)

// ProfileSpec is a parsed --model-profile value.
type ProfileSpec struct {
	Name     string
	Provider Provider
	ModelID  string
}

// ParseProfile parses name=provider:model-id, splitting each delimiter once.
func ParseProfile(value string) (ProfileSpec, error) {
	name, target, ok := strings.Cut(strings.TrimSpace(value), "=")
	if !ok || strings.TrimSpace(name) == "" {
		return ProfileSpec{}, errors.New("model profile must use name=provider:model-id")
	}
	provider, modelID, ok := strings.Cut(target, ":")
	if !ok || strings.TrimSpace(modelID) == "" {
		return ProfileSpec{}, errors.Errorf("model profile %q must include a provider and model ID", strings.TrimSpace(name))
	}
	spec := ProfileSpec{Name: strings.TrimSpace(name), Provider: Provider(strings.TrimSpace(provider)), ModelID: strings.TrimSpace(modelID)}
	if !supportedProvider(spec.Provider) {
		return ProfileSpec{}, errors.Errorf("model profile %q uses unsupported provider %q", spec.Name, spec.Provider)
	}
	return spec, nil
}

// ParseProfiles parses profile flags and rejects duplicate names.
func ParseProfiles(values []string) ([]ProfileSpec, error) {
	seen := make(map[string]struct{}, len(values))
	profiles := make([]ProfileSpec, 0, len(values))
	for _, value := range values {
		spec, err := ParseProfile(value)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[spec.Name]; ok {
			return nil, errors.Errorf("duplicate model profile %q", spec.Name)
		}
		seen[spec.Name] = struct{}{}
		profiles = append(profiles, spec)
	}
	if len(profiles) == 0 {
		return nil, errors.New("at least one model profile is required")
	}
	return profiles, nil
}

func supportedProvider(provider Provider) bool {
	switch provider {
	case ProviderGoogleAI, ProviderVertex, ProviderNVIDIANIM, ProviderOpenRouter:
		return true
	default:
		return false
	}
}

// Selection identifies deployment defaults.
type Selection struct {
	Primary  string
	Fallback string
}

// Registry contains validated profiles and their hosts.
type Registry struct {
	profiles  map[string]Profile
	hosts     map[string]Host
	selection Selection
}

// NewRegistry validates already-probed profiles and selections.
func NewRegistry(profiles []Profile, hosts map[string]Host, selection Selection) (*Registry, error) {
	registry := &Registry{profiles: make(map[string]Profile, len(profiles)), hosts: make(map[string]Host, len(hosts)), selection: selection}
	for _, profile := range profiles {
		if strings.TrimSpace(profile.Name) == "" || strings.TrimSpace(profile.ModelID) == "" || !supportedProvider(profile.Provider) {
			return nil, errors.New("invalid model profile")
		}
		if _, ok := registry.profiles[profile.Name]; ok {
			return nil, errors.Errorf("duplicate model profile %q", profile.Name)
		}
		registry.profiles[profile.Name] = profile
		if host := hosts[profile.Name]; host != nil {
			registry.hosts[profile.Name] = host
		}
	}
	if err := registry.validateSelection(); err != nil {
		return nil, err
	}
	return registry, nil
}

func (r *Registry) validateSelection() error {
	primary, ok := r.profiles[r.selection.Primary]
	if !ok {
		return errors.Errorf("primary model profile %q does not exist", r.selection.Primary)
	}
	if !primary.ToolsEnabled() {
		return errors.Errorf("primary model profile %q must confirm tools and tool choice", r.selection.Primary)
	}
	if r.selection.Fallback != "" {
		if _, ok := r.profiles[r.selection.Fallback]; !ok {
			return errors.Errorf("fallback model profile %q does not exist", r.selection.Fallback)
		}
		if r.selection.Fallback == r.selection.Primary {
			return errors.New("primary and fallback model profiles must be different")
		}
	}
	return nil
}

// Profile returns a defensive profile copy.
func (r *Registry) Profile(name string) (Profile, bool) {
	profile, ok := r.profiles[name]
	return profile, ok
}

// Host returns the host assigned to a profile.
func (r *Registry) Host(name string) (Host, bool) {
	host, ok := r.hosts[name]
	return host, ok
}

// Profiles returns profiles ordered by name.
func (r *Registry) Profiles() []Profile {
	names := make([]string, 0, len(r.profiles))
	for name := range r.profiles {
		names = append(names, name)
	}
	slices.Sort(names)
	result := make([]Profile, 0, len(names))
	for _, name := range names {
		result = append(result, r.profiles[name])
	}
	return result
}

// Selection returns deployment defaults.
func (r *Registry) Selection() Selection { return r.selection }

// Resolve validates request-scoped selectors, using defaults for stale aliases.
func (r *Registry) Resolve(primary, fallback string) (Profile, *Profile, bool) {
	stale := false
	if strings.TrimSpace(primary) == "" {
		primary = r.selection.Primary
	}
	primaryProfile, ok := r.profiles[primary]
	if !ok || !primaryProfile.ToolsEnabled() {
		primaryProfile = r.profiles[r.selection.Primary]
		fallback = r.selection.Fallback
		stale = true
	}
	if fallback == "" {
		return primaryProfile, nil, stale
	}
	fallbackProfile, ok := r.profiles[fallback]
	if !ok || fallback == primaryProfile.Name {
		stale = true
		fallback = r.selection.Fallback
		fallbackProfile, ok = r.profiles[fallback]
	}
	if !ok || fallbackProfile.Name == primaryProfile.Name {
		return primaryProfile, nil, stale
	}
	return primaryProfile, &fallbackProfile, stale
}

// ProbeProfiles probes each unique provider/model concurrently and aggregates failures.
func ProbeProfiles(ctx context.Context, profiles []Profile, probers map[Provider]Prober, timeout time.Duration) ([]Profile, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	type result struct {
		key  string
		caps Capabilities
		err  error
	}
	unique := make(map[string]Profile)
	for _, profile := range profiles {
		unique[string(profile.Provider)+"\x00"+profile.ModelID] = profile
	}
	results := make(chan result, len(unique))
	var wait sync.WaitGroup
	for key, profile := range unique {
		key, profile := key, profile
		wait.Add(1)
		go func() {
			defer wait.Done()
			prober := probers[profile.Provider]
			if prober == nil {
				results <- result{key: key, err: errors.Errorf("provider %s is not configured", profile.Provider)}
				return
			}
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			caps, err := prober.Probe(probeCtx, profile)
			results <- result{key: key, caps: caps, err: err}
		}()
	}
	wait.Wait()
	close(results)
	capabilities := make(map[string]Capabilities, len(unique))
	var failures []string
	for result := range results {
		if result.err != nil {
			profile := unique[result.key]
			failures = append(failures, fmt.Sprintf("%s:%s: %v", profile.Provider, profile.ModelID, result.err))
			continue
		}
		capabilities[result.key] = result.caps
	}
	if len(failures) > 0 {
		slices.Sort(failures)
		return nil, errors.Errorf("model profile validation failed: %s", strings.Join(failures, "; "))
	}
	resultProfiles := make([]Profile, len(profiles))
	for i, profile := range profiles {
		profile.Capabilities = capabilities[string(profile.Provider)+"\x00"+profile.ModelID]
		resultProfiles[i] = profile
	}
	return resultProfiles, nil
}
