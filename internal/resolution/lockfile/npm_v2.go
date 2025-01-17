package lockfile

import (
	"cmp"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"deps.dev/util/resolve"
	"github.com/google/osv-scanner/internal/resolution/datasource"
	"github.com/google/osv-scanner/pkg/lockfile"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/exp/maps"
)

// New-style (npm >= 7 / lockfileVersion 2+) structure
// https://docs.npmjs.com/cli/v9/configuring-npm/package-lock-json
// Installed packages are in the flat "packages" object, keyed by the install path
// e.g. "node_modules/foo/node_modules/bar"
// packages contain most information from their own manifests.
func (rw NpmLockfileIO) nodesFromPackages(lockJSON lockfile.NpmLockfile) (*resolve.Graph, *npmNodeModule, error) {
	var g resolve.Graph
	// Create graph nodes and reconstruct the node_modules folder structure in memory
	root, ok := lockJSON.Packages[""]
	if !ok {
		return nil, nil, errors.New("missing root node")
	}
	nID := g.AddNode(resolve.VersionKey{
		PackageKey: resolve.PackageKey{
			System: resolve.NPM,
			Name:   root.Name,
		},
		VersionType: resolve.Concrete,
		Version:     root.Version,
	})
	nodeModuleTree := rw.makeNodeModuleDeps(root, true)
	nodeModuleTree.NodeID = nID

	// paths for npm workspace subfolders, not inside root node_modules
	workspaceModules := make(map[string]*npmNodeModule)
	workspaceModules[""] = nodeModuleTree

	// iterate keys by node_modules depth
	for _, k := range rw.packageNamesByNodeModuleDepth(lockJSON.Packages) {
		if k == "" {
			// skip the root node
			continue
		}
		pkg, ok := lockJSON.Packages[k]
		if !ok {
			panic("key not in packages")
		}
		path := strings.Split(k, "node_modules/")
		if len(path) == 1 {
			// the path does not contain "node_modules/", assume this is a workspace directory
			nID := g.AddNode(resolve.VersionKey{
				PackageKey: resolve.PackageKey{
					System: resolve.NPM,
					Name:   path[0], // This will get replaced by the name from the symlink
				},
				VersionType: resolve.Concrete,
				Version:     pkg.Version,
			})
			m := rw.makeNodeModuleDeps(pkg, true) // NB: including the dev dependencies
			m.NodeID = nID
			workspaceModules[path[0]] = m

			continue
		}

		if pkg.Link {
			// This is the symlink to the workspace directory in node_modules
			if len(path) != 2 || path[0] != "" {
				// Not sure what situation would lead to this
				panic("Found symlink in package-lock.json that's not in root node_modules directory")
			}
			m := workspaceModules[pkg.Resolved]
			if m == nil {
				// Can symlinks show up without workspaces?
				panic("symlink in package-lock.json processed before real directory")
			}

			// attach the workspace to the tree
			pkgName := path[1]
			nodeModuleTree.Children[pkgName] = m
			if pkg.Resolved == "" {
				// weird case: the root directory is symlinked into its own node_modules
				continue
			}
			m.Parent = nodeModuleTree

			// rename the node to the name it would be referred to as in package.json
			g.Nodes[m.NodeID].Version.Name = pkgName
			// add it as a dependency of the root node, so it's not orphaned
			if _, ok := nodeModuleTree.Deps[pkgName]; !ok {
				nodeModuleTree.Deps[pkgName] = "*"
			}

			continue
		}

		// find the direct parent package by traversing the path
		parent := nodeModuleTree
		if path[0] != "" {
			// jump to the corresponding workspace if package is in one
			parent = workspaceModules[strings.TrimSuffix(path[0], "/")]
		}
		for _, p := range path[1 : len(path)-1] { // skip root directory
			p = strings.TrimSuffix(p, "/")
			parent = parent.Children[p]
		}

		name := path[len(path)-1]
		nID := g.AddNode(resolve.VersionKey{
			PackageKey: resolve.PackageKey{
				System: resolve.NPM,
				Name:   name,
			},
			VersionType: resolve.Concrete,
			Version:     pkg.Version,
		})
		parent.Children[name] = rw.makeNodeModuleDeps(pkg, false)
		parent.Children[name].NodeID = nID
		parent.Children[name].Parent = parent
		parent.Children[name].ActualName = pkg.Name
	}

	return &g, nodeModuleTree, nil
}

func (rw NpmLockfileIO) makeNodeModuleDeps(pkg lockfile.NpmLockPackage, includeDev bool) *npmNodeModule {
	deps := make(map[string]string)
	maps.Copy(deps, pkg.Dependencies)
	if includeDev {
		maps.Copy(deps, pkg.DevDependencies)
	}
	optDeps := make(map[string]string)
	maps.Copy(optDeps, pkg.OptionalDependencies)
	// Some versions of npm apparently do not automatically install peerDependencies, so treat them as optional
	maps.Copy(optDeps, pkg.PeerDependencies)
	rw.reVersionAliasedDeps(deps)
	rw.reVersionAliasedDeps(optDeps)

	return &npmNodeModule{
		Children:     make(map[string]*npmNodeModule),
		Deps:         deps,
		OptionalDeps: optDeps,
	}
}

func (rw NpmLockfileIO) packageNamesByNodeModuleDepth(packages map[string]lockfile.NpmLockPackage) []string {
	keys := maps.Keys(packages)
	slices.SortFunc(keys, func(a, b string) int {
		aSplit := strings.Split(a, "node_modules/")
		bSplit := strings.Split(b, "node_modules/")
		if c := cmp.Compare(len(aSplit), len(bSplit)); c != 0 {
			return c
		}
		// sort alphabetically if they're the same depth
		return cmp.Compare(a, b)
	})

	return keys
}

func (rw NpmLockfileIO) modifyPackageLockPackages(lockJSON string, patches map[string]map[string]string, api *datasource.NpmRegistryAPIClient) (string, error) {
	packages := gjson.Get(lockJSON, "packages")
	if !packages.Exists() {
		return lockJSON, nil
	}

	for key, value := range packages.Map() {
		parts := strings.Split(key, "node_modules/")
		if len(parts) == 0 {
			continue
		}
		pkg := parts[len(parts)-1]
		if n := value.Get("name"); n.Exists() { // if this is an alias, use the real package as the name
			pkg = n.String()
		}
		if upgrades, ok := patches[pkg]; ok {
			if newVer, ok := upgrades[value.Get("version").String()]; ok {
				fullPath := "packages." + strings.ReplaceAll(key, ".", "\\.")
				var err error
				if lockJSON, err = rw.updatePackage(lockJSON, fullPath, pkg, newVer, api); err != nil {
					return lockJSON, err
				}
			}
		}
	}

	return lockJSON, nil
}

func (rw NpmLockfileIO) updatePackage(jsonText, jsonPath, packageName, newVersion string, api *datasource.NpmRegistryAPIClient) (string, error) {
	npmData, err := api.FullJSON(context.Background(), packageName, newVersion)
	if err != nil {
		return "", err
	}

	// The "dependencies" returned from the registry includes (can include?) both optional and regular dependencies
	// But the "optionalDependencies" are (always?) removed from "dependencies" package-lock.json.
	for _, opt := range npmData.Get("optionalDependencies|@keys").Array() {
		s, _ := sjson.Delete(npmData.Raw, "dependencies."+opt.String())
		npmData = gjson.Parse(s)
	}

	// I can't find a consistent list of what fields should be included in package-lock.json packages
	// https://docs.npmjs.com/cli/v9/configuring-npm/package-lock-json#packages seems list some
	// but I've seen fields not listed there get included, and fields that it says to include (e.g. license) missing;
	// Might fill in as much of package.json? https://docs.npmjs.com/cli/v9/configuring-npm/package-json
	// It also seems to depend on npm version?
	// Instead, just modify the fields that are present
	for _, key := range gjson.Get(jsonText, jsonPath+"|@keys").Array() {
		switch key.String() {
		case "resolved":
			jsonText, _ = sjson.Set(jsonText, jsonPath+".resolved", npmData.Get("dist.tarball").String())
		case "integrity":
			jsonText, _ = sjson.Set(jsonText, jsonPath+".integrity", npmData.Get("dist.integrity").String())
		case "bin":
			// the api formats the paths as "./path/to", while package-lock.json seem to use "path/to"
			// TODO: smarter way for indentation
			newVal := npmData.Get(key.String() + "|@pretty:{\"prefix\": \"      \"}")
			if newVal.Exists() {
				text := newVal.Raw
				// remove trailing newlines that @pretty creates for objects
				text = strings.TrimSuffix(text, "\n")
				for k, v := range newVal.Map() {
					text, _ = sjson.Set(text, k, filepath.Clean(v.String()))
				}
				jsonText, _ = sjson.SetRaw(jsonText, jsonPath+".bin", text)
			} else {
				// explicitly remove it if it's no longer present
				jsonText, _ = sjson.Delete(jsonText, jsonPath+".bin")
			}
		// if all dependencies have been removed, explicitly remove the field
		case "dependencies":
			fallthrough
		case "devDependencies": // shouldn't show up in package-lock.json
			fallthrough
		case "peerDependencies":
			fallthrough
		case "optionalDependencies":
			if !npmData.Get(key.String()).Exists() {
				// TODO: Think of the orphaned children
				jsonText, _ = sjson.Delete(jsonText, jsonPath+"."+key.String())
				continue
			}

			fallthrough
		default:
			// use @pretty to format objects correctly & with correct indentation
			// TODO: smarter way for indentation
			newVal := npmData.Get(key.String() + "|@pretty:{\"prefix\": \"      \"}")
			if newVal.Exists() {
				text := newVal.Raw
				// remove trailing newlines that @pretty creates for objects
				text = strings.TrimSuffix(text, "\n")
				jsonText, _ = sjson.SetRaw(jsonText, jsonPath+"."+key.String(), text)
			}
			// if it doesn't exist, assume it's one of the package-lock flags e.g. "dev"
			// TODO: It could be a removed field
		}
	}

	return jsonText, nil
}
