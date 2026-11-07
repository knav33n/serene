package nse

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"calm/internal/market"
)

// Index groups, matching the site's categories.
const (
	GroupBroad    = "broad"
	GroupSectoral = "sectoral"
	GroupThematic = "thematic"
	GroupStrategy = "strategy"
	GroupOther    = "other" // bonds and non-equity series
)

// Entry is one index in the catalog.
type Entry struct {
	Name    string   `yaml:"name"`
	Group   string   `yaml:"group"`
	Breadth bool     `yaml:"breadth"` // counts in the sector breadth check
	Aliases []string `yaml:"aliases"`
}

// Catalog resolves the names NSE has used over the years. Names are matched
// ignoring case, since NSE has changed the case of some without changing the index.
type Catalog struct {
	Entries []Entry `yaml:"indices"`
	byName  map[string]*Entry
}

func fold(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// LoadCatalog reads the catalog file. A name claimed twice is an error, as it
// would join two different series into one.
func LoadCatalog(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Catalog{}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, c.index()
}

func (c *Catalog) index() error {
	c.byName = map[string]*Entry{}
	for i := range c.Entries {
		e := &c.Entries[i]
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			if old, dup := c.byName[fold(n)]; dup {
				return fmt.Errorf("catalog: %q is claimed by both %q and %q", n, old.Name, e.Name)
			}
			c.byName[fold(n)] = e
		}
	}
	return nil
}

// Lookup returns the entry for a published name.
func (c *Catalog) Lookup(name string) (*Entry, bool) {
	e, ok := c.byName[fold(name)]
	return e, ok
}

// Group returns the group of a canonical name, or empty if it isn't catalogued.
func (c *Catalog) Group(name string) string {
	if e, ok := c.Lookup(name); ok {
		return e.Group
	}
	return ""
}

// Apply sets each row's canonical name and returns the rows to keep. An unknown
// name stays as published so that nothing is dropped, and is returned for the
// report to list. A name repeated within a file keeps its first row, as seen on
// 2013-02-08 when CNX Alpha Index appears twice and CNX High Beta not at all.
// Two different names resolving to one index on the same day mean an alias is
// wrong, which is an error.
func (c *Catalog) Apply(rows []market.IndexDay) (kept []market.IndexDay, unknown []string, err error) {
	seen := map[string]string{}
	kept = rows[:0:0]
	for _, r := range rows {
		if e, ok := c.Lookup(r.Source); ok {
			r.Name = e.Name
		} else {
			unknown = append(unknown, r.Source)
		}
		if other, dup := seen[r.Name]; dup {
			if other == r.Source {
				continue
			}
			return nil, nil, fmt.Errorf("%s: %q and %q both resolve to %q", r.Date, other, r.Source, r.Name)
		}
		seen[r.Name] = r.Source
		kept = append(kept, r)
	}
	return kept, unknown, nil
}
