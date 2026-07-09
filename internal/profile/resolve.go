package profile

import (
	"fmt"
	"strings"

	"github.com/lestrrat-go/ccc/internal/config"
)

// Source records how a profile was chosen, so ccc can tell the user which
// account it is about to run as.
type Source string

const (
	SourceFlag    Source = "--profile"
	SourceDirFile Source = config.DirConfigName
	SourceDefault Source = "default_profile"
)

// Resolution is a resolved profile and its provenance.
type Resolution struct {
	Name   string
	Source Source
	// Origin is the .ccc.toml path, when Source is SourceDirFile.
	Origin string
}

// String renders the resolution for the stderr banner.
func (r Resolution) String() string {
	if r.Origin != "" {
		return fmt.Sprintf("%s (via %s)", r.Name, r.Origin)
	}
	return fmt.Sprintf("%s (via %s)", r.Name, r.Source)
}

// Resolve picks a profile: flag, then the nearest .ccc.toml, then
// default_profile. Never guesses — an unresolvable profile is an error listing
// what is available, because a wrong-account run is worse than a failed one.
func (s *Store) Resolve(flag string, cfg *config.Config, cwd string) (Resolution, error) {
	res, err := s.resolveName(flag, cfg, cwd)
	if err != nil {
		return Resolution{}, err
	}
	if err := ValidateName(res.Name); err != nil {
		return Resolution{}, err
	}
	if !s.Exists(res.Name) {
		return Resolution{}, fmt.Errorf("profile %q (from %s) %w\n%s",
			res.Name, res.Source, ErrNotExist, s.available())
	}
	return res, nil
}

func (s *Store) resolveName(flag string, cfg *config.Config, cwd string) (Resolution, error) {
	if flag != "" {
		return Resolution{Name: flag, Source: SourceFlag}, nil
	}

	name, origin, ok, err := config.FindDir(cwd)
	if err != nil {
		return Resolution{}, err
	}
	if ok {
		return Resolution{Name: name, Source: SourceDirFile, Origin: origin}, nil
	}

	if cfg.DefaultProfile != "" {
		return Resolution{Name: cfg.DefaultProfile, Source: SourceDefault}, nil
	}

	return Resolution{}, fmt.Errorf("no profile selected: pass --profile, add a %s, or set default_profile\n%s\nrun `ccc --help` for usage",
		config.DirConfigName, s.available())
}

func (s *Store) available() string {
	names, err := s.List()
	if err != nil || len(names) == 0 {
		return "no profiles yet: create one with `ccc profile create <name>`"
	}
	return "available profiles: " + strings.Join(names, ", ")
}
