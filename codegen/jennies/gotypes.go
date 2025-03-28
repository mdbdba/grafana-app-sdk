package jennies

import (
	"context"
	"fmt"
	"path"
	"strings"

	"cuelang.org/go/cue"
	"github.com/grafana/codejen"
	"github.com/grafana/cog"

	"github.com/grafana/grafana-app-sdk/codegen"
)

const GoTypesMaxDepth = 5

// GoTypes is a Jenny for turning a codegen.Kind into go types according to its codegen settings.
type GoTypes struct {
	// GenerateOnlyCurrent should be set to true if you only want to generate code for the kind.Properties().Current version.
	// This will affect the package and path(s) of the generated file(s).
	GenerateOnlyCurrent bool

	// Depth represents the tree depth for creating go types from fields. A Depth of 0 will return one go type
	// (plus any definitions used by that type), a Depth of 1 will return a file with a go type for each top-level field
	// (plus any definitions encompassed by each type), etc. Note that types are _not_ generated for fields above the Depth
	// level--i.e. a Depth of 1 will generate go types for each field within the KindVersion.Schema, but not a type for the
	// Schema itself. Because Depth results in recursive calls, the highest value is bound to a max of GoTypesMaxDepth.
	Depth int

	// NamingDepth determines how types are named in relation to Depth. If Depth <= NamingDepth, the go types are named
	// using the field name of the type. Otherwise, Names used are prefixed by field names between Depth and NamingDepth.
	// Typically, a value of 0 is "safest" for NamingDepth, as it prevents overlapping names for types.
	// However, if you know that your fields have unique names up to a certain depth, you may configure this to be higher.
	NamingDepth int

	// AddKubernetesCodegen toggles whether kubernetes codegen comments are added to the go types generated by this jenny.
	// The codegen comments can then be used with the OpenAPI jenny, or with the kubernetes codegen tooling.
	AddKubernetesCodegen bool

	// GroupByKind determines whether kinds are grouped by GroupVersionKind or just GroupVersion.
	// If GroupByKind is true, generated paths are <kind>/<version>/<file>, instead of the default <version>/<file>.
	// When GroupByKind is false, subresource types (such as spec and status) are prefixed with the kind name,
	// i.e. generating FooSpec instead of Spec for kind.Name() = "Foo" and Depth=1
	GroupByKind bool

	// AnyAsInterface determines whether to use `interface{}` instead of `any` in generated go code.
	// If true, `interface{}` will be used instead of `any`.
	AnyAsInterface bool
}

func (*GoTypes) JennyName() string {
	return "GoTypes"
}

func (g *GoTypes) Generate(kind codegen.Kind) (codejen.Files, error) {
	if g.GenerateOnlyCurrent {
		ver := kind.Version(kind.Properties().Current)
		if ver == nil {
			return nil, fmt.Errorf("version '%s' of kind '%s' does not exist", kind.Properties().Current, kind.Name())
		}
		return g.generateFiles(ver, kind.Name(), kind.Properties().MachineName, kind.Properties().MachineName, kind.Properties().MachineName)
	}

	files := make(codejen.Files, 0)
	versions := kind.Versions()
	for i := 0; i < len(versions); i++ {
		ver := versions[i]
		if !ver.Codegen.Go.Enabled {
			continue
		}

		generated, err := g.generateFiles(&ver, kind.Name(), kind.Properties().MachineName, ToPackageName(ver.Version), GetGeneratedPath(g.GroupByKind, kind, ver.Version))
		if err != nil {
			return nil, err
		}
		files = append(files, generated...)
	}

	return files, nil
}

func (g *GoTypes) generateFiles(version *codegen.KindVersion, name string, machineName string, packageName string, pathPrefix string) (codejen.Files, error) {
	if g.Depth > 0 {
		namePrefix := ""
		if !g.GroupByKind {
			namePrefix = exportField(name)
		}
		return g.generateFilesAtDepth(version.Schema, version, 0, machineName, packageName, pathPrefix, namePrefix)
	}

	codegenPipeline := cog.TypesFromSchema().
		CUEValue(packageName, version.Schema, cog.ForceEnvelope(name)).
		Golang(cog.GoConfig{
			AnyAsInterface:                g.AnyAsInterface,
			AllowMarshalEmptyDisjunctions: version.Codegen.Go.Config.AllowMarshalEmptyDisjunctions,
		})

	if g.AddKubernetesCodegen {
		codegenPipeline = codegenPipeline.SchemaTransformations(cog.AppendCommentToObjects("+k8s:openapi-gen=true"))
	}

	files, err := codegenPipeline.Run(context.Background())
	if err != nil {
		return nil, err
	}

	if len(files) != 1 {
		return nil, fmt.Errorf("expected one file to be generated, got %d", len(files))
	}

	return codejen.Files{codejen.File{
		Data:         files[0].Data,
		RelativePath: fmt.Sprintf(path.Join(pathPrefix, "%s_gen.go"), strings.ToLower(machineName)),
		From:         []codejen.NamedJenny{g},
	}}, nil
}

//nolint:goconst
func (g *GoTypes) generateFilesAtDepth(v cue.Value, kv *codegen.KindVersion, currDepth int, machineName string, packageName string, pathPrefix string, namePrefix string) (codejen.Files, error) {
	if currDepth == g.Depth {
		fieldName := make([]string, 0)
		for _, s := range TrimPathPrefix(v.Path(), kv.Schema.Path()).Selectors() {
			fieldName = append(fieldName, s.String())
		}

		goBytes, err := GoTypesFromCUE(v, CUEGoConfig{
			PackageName:                    packageName,
			Name:                           exportField(strings.Join(fieldName, "")),
			NamePrefix:                     namePrefix,
			AddKubernetesOpenAPIGenComment: g.AddKubernetesCodegen && !(len(fieldName) == 1 && fieldName[0] == "metadata"),
			AnyAsInterface:                 g.AnyAsInterface,
			AllowMarshalEmptyDisjunctions:  kv.Codegen.Go.Config.AllowMarshalEmptyDisjunctions,
		}, len(v.Path().Selectors())-(g.Depth-g.NamingDepth))
		if err != nil {
			return nil, err
		}

		return codejen.Files{codejen.File{
			Data:         goBytes,
			RelativePath: fmt.Sprintf(path.Join(pathPrefix, "%s_%s_gen.go"), strings.ToLower(machineName), strings.Join(fieldName, "_")),
			From:         []codejen.NamedJenny{g},
		}}, nil
	}

	it, err := v.Fields()
	if err != nil {
		return nil, err
	}

	files := make(codejen.Files, 0)
	for it.Next() {
		f, err := g.generateFilesAtDepth(it.Value(), kv, currDepth+1, machineName, packageName, pathPrefix, namePrefix)
		if err != nil {
			return nil, err
		}
		files = append(files, f...)
	}
	return files, nil
}

type CUEGoConfig struct {
	PackageName string
	Name        string

	AddKubernetesOpenAPIGenComment bool
	AnyAsInterface                 bool
	AllowMarshalEmptyDisjunctions  bool

	// NamePrefix prefixes all generated types with the provided NamePrefix
	NamePrefix string
}

func GoTypesFromCUE(v cue.Value, cfg CUEGoConfig, maxNamingDepth int) ([]byte, error) {
	nameFunc := func(_ cue.Value, definitionPath cue.Path) string {
		i := 0
		for ; i < len(definitionPath.Selectors()) && i < len(v.Path().Selectors()); i++ {
			if maxNamingDepth > 0 && i >= maxNamingDepth {
				break
			}
			if !SelEq(definitionPath.Selectors()[i], v.Path().Selectors()[i]) {
				break
			}
		}
		if i > 0 {
			definitionPath = cue.MakePath(definitionPath.Selectors()[i:]...)
		}
		return strings.Trim(definitionPath.String(), "?#")
	}

	codegenPipeline := cog.TypesFromSchema().
		CUEValue(cfg.PackageName, v, cog.ForceEnvelope(cfg.Name), cog.NameFunc(nameFunc)).
		SchemaTransformations(cog.PrefixObjectsNames(cfg.NamePrefix)).
		Golang(cog.GoConfig{
			AnyAsInterface:                cfg.AnyAsInterface,
			AllowMarshalEmptyDisjunctions: cfg.AllowMarshalEmptyDisjunctions,
		})

	if cfg.AddKubernetesOpenAPIGenComment {
		codegenPipeline = codegenPipeline.SchemaTransformations(cog.AppendCommentToObjects("+k8s:openapi-gen=true"))
	}

	files, err := codegenPipeline.Run(context.Background())
	if err != nil {
		return nil, err
	}

	if len(files) != 1 {
		return nil, fmt.Errorf("expected one file to be generated, got %d", len(files))
	}

	return files[0].Data, nil
}

// SanitizeLabelString strips characters from a string that are not allowed for
// use in a CUE label.
func sanitizeLabelString(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			fallthrough
		case r >= 'A' && r <= 'Z':
			fallthrough
		case r >= '0' && r <= '9':
			fallthrough
		case r == '_':
			return r
		default:
			return -1
		}
	}, s)
}

// exportField makes a field name exported
func exportField(field string) string {
	if len(field) > 0 {
		return strings.ToUpper(field[:1]) + field[1:]
	}
	return strings.ToUpper(field)
}
