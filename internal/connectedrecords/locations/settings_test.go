package locations

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// location-name-localities downloads where the store reads, which only the
// server's setting names, so its directory has no default.
func TestLocalityNamingRequiresTheGeonamesDirectory(t *testing.T) {
	registry, err := NewLocalityNamingSettingsRegistry(servicesettings.MapEnvironment(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CheckRequired(); err == nil || !strings.Contains(err.Error(), "LOCATION_STORE_GEONAMES_DIRECTORY") {
		t.Fatalf("no GeoNames directory = %v, want a refusal naming LOCATION_STORE_GEONAMES_DIRECTORY", err)
	}
	registry, err = NewLocalityNamingSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"LOCATION_STORE_GEONAMES_DIRECTORY": "/srv/example/geonames"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CheckRequired(); err != nil {
		t.Fatal(err)
	}
	if got := registry.String(SettingGeonamesDirectory); got != "/srv/example/geonames" {
		t.Errorf("GeoNames directory = %q, want the one set", got)
	}
}

// Every environment variable the package's code and commands read by name is
// declared.
func TestEveryEnvironmentVariableThePackageReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range append(append(append(ImportGoogleSettingDefinitions(), ImportPhotosSettingDefinitions()...),
		LocalityNamingSettingDefinitions()...), append(append(append(append(JourneyGroupingSettingDefinitions(), CalendarMatchingSettingDefinitions()...), PlaceNameSuggestingSettingDefinitions()...),
		ImportActivitySettingDefinitions()...), append(ImportPhotoSidecarsSettingDefinitions(), PhoneSegmentingSettingDefinitions()...)...)...) {
		declared[definition.EnvironmentVariable] = true
	}
	filesRead := 0
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		filesRead++
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall || len(call.Args) == 0 {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			packageName, isIdentifier := selector.X.(*ast.Ident)
			if !isIdentifier || packageName.Name != "os" || (selector.Sel.Name != "Getenv" && selector.Sel.Name != "LookupEnv") {
				return true
			}
			literal, isLiteral := call.Args[0].(*ast.BasicLit)
			if !isLiteral {
				t.Errorf("%s reads an environment variable whose name is computed", path)
				return true
			}
			if name, _ := strconv.Unquote(literal.Value); !declared[name] {
				t.Errorf("%s reads %s, which SettingDefinitions does not declare", path, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join("cmd", "location-name-localities", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 5 {
		t.Fatalf("the scan read %d files; it is not looking at the package", filesRead)
	}
}
