package k8s

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// DiscoveryTTL is how long discovery results are cached.
const DiscoveryTTL = 10 * time.Minute

// Resource is one API resource a kind resolves to.
type Resource struct {
	GVR        schema.GroupVersionResource
	Kind       string
	Namespaced bool
	Verbs      []string
	ShortNames []string
	Singular   string
	preferred  bool // in its group's preferred version
}

// Resolver maps what people type for a kind (po, pods, Pod,
// deployments.apps, apps/v1/Deployment) to API resources, from cached
// discovery.
type Resolver struct {
	disc discovery.DiscoveryInterface
	Now  func() time.Time

	mu       sync.Mutex
	at       time.Time
	forcedAt time.Time // last refresh for an unknown kind
	list     []Resource
	groups   map[string]bool
}

func NewResolver(disc discovery.DiscoveryInterface) *Resolver { return &Resolver{disc: disc} }

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// unknownRefreshEvery bounds how often an unknown kind refreshes discovery.
const unknownRefreshEvery = 30 * time.Second

// resources returns the cached discovery, refreshing it if stale, or if
// force and the last forced refresh was unknownRefreshEvery ago.
func (r *Resolver) resources(force bool) ([]Resource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if force && r.list != nil {
		if !r.forcedAt.IsZero() && r.now().Sub(r.forcedAt) < unknownRefreshEvery {
			force = false
		} else {
			r.forcedAt = r.now()
		}
	}
	if !force && r.list != nil && r.now().Sub(r.at) < DiscoveryTTL {
		return r.list, nil
	}
	groups, lists, err := r.disc.ServerGroupsAndResources()
	if err != nil && len(lists) == 0 {
		// Partial results (one aggregated API down) are still useful.
		return nil, fmt.Errorf("discovery: %w", err)
	}
	preferred := map[string]string{} // group → preferred version
	r.groups = map[string]bool{}
	for _, g := range groups {
		if g != nil {
			preferred[g.Name] = g.PreferredVersion.Version
			r.groups[g.Name] = true
		}
	}
	var out []Resource
	for _, l := range lists {
		if l == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		r.groups[gv.Group] = true
		for _, res := range l.APIResources {
			if strings.Contains(res.Name, "/") {
				continue // subresource
			}
			pv, ok := preferred[gv.Group]
			out = append(out, Resource{
				GVR:  gv.WithResource(res.Name),
				Kind: res.Kind, Namespaced: res.Namespaced, Verbs: res.Verbs, ShortNames: res.ShortNames,
				Singular: res.SingularName, preferred: !ok || pv == gv.Version,
			})
		}
	}
	// Core first, then by group, so "events" is the core kind.
	sort.SliceStable(out, func(i, j int) bool {
		gi, gj := out[i].GVR.Group, out[j].GVR.Group
		if (gi == "") != (gj == "") {
			return gi == ""
		}
		return gi < gj
	})
	r.list, r.at = out, r.now()
	return out, nil
}

// HasGroup reports whether the cluster serves an API group (e.g.
// metrics.k8s.io).
func (r *Resolver) HasGroup(group string) bool {
	if _, err := r.resources(false); err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.groups[group]
}

// Resolve finds the resource for a kind as typed by a person: a kind
// (Pod), a resource name (pods), a singular (pod), a short name (po),
// resource.group (deployments.apps) or group/version/kind
// (apps/v1/Deployment, v1/Pod). Unknown kinds refresh discovery once.
func (r *Resolver) Resolve(kind string) (Resource, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return Resource{}, fmt.Errorf("kind is required")
	}
	for _, force := range []bool{false, true} {
		list, err := r.resources(force)
		if err != nil {
			return Resource{}, err
		}
		if res, ok := match(list, kind); ok {
			return res, nil
		}
	}
	return Resource{}, fmt.Errorf("unknown kind %q (not served by this cluster)", kind)
}

// ForKind finds the resource for an apiVersion and kind (as in a manifest).
func (r *Resolver) ForKind(apiVersion, kind string) (Resource, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return Resource{}, err
	}
	for _, force := range []bool{false, true} {
		list, err := r.resources(force)
		if err != nil {
			return Resource{}, err
		}
		for _, res := range list {
			if res.GVR.GroupVersion() == gv && res.Kind == kind {
				return res, nil
			}
		}
	}
	return Resource{}, fmt.Errorf("unknown kind %s %s (not served by this cluster)", apiVersion, kind)
}

func match(list []Resource, kind string) (Resource, bool) {
	if parts := strings.Split(kind, "/"); len(parts) > 1 {
		// group/version/kind or version/kind
		var gv schema.GroupVersion
		switch len(parts) {
		case 2:
			gv = schema.GroupVersion{Version: parts[0]}
		case 3:
			gv = schema.GroupVersion{Group: parts[0], Version: parts[1]}
		default:
			return Resource{}, false
		}
		name := parts[len(parts)-1]
		for _, res := range list {
			if res.GVR.GroupVersion() == gv && names(res, name) {
				return res, true
			}
		}
		return Resource{}, false
	}
	name, group, dotted := strings.Cut(kind, ".")
	for _, res := range list {
		if !res.preferred {
			continue
		}
		if dotted && res.GVR.Group != strings.ToLower(group) {
			continue
		}
		if names(res, name) {
			return res, true
		}
	}
	return Resource{}, false
}

// names reports whether name is one of res's names (any case).
func names(res Resource, name string) bool {
	n := strings.ToLower(name)
	if n == strings.ToLower(res.Kind) || n == res.GVR.Resource || n == res.Singular {
		return true
	}
	for _, s := range res.ShortNames {
		if n == s {
			return true
		}
	}
	return false
}
