// Command parity-check reports provider behaviours that have no miner
// counterpart.
//
// The miner is a port of the provider, so a behaviour that exists in one tree
// and not the other is either an unfinished port or a silent drift. Reading
// both trees by hand does not scale to hundreds of files, so this tool makes
// the comparison mechanical:
//
//   - files are paired by stem, by an explicit mapping, or — because ports
//     rename, split and consolidate files — by shared top-level declarations;
//     a provider file may pair with several miner files at once;
//   - top-level functions and types are paired by exact name, by the
//     Provider/Miner substitution (including an export change), or by an
//     explicit mapping;
//   - provider-only gaps fail the run unless they are listed, with a reason,
//     in the allowlist;
//   - miner-only files are reported separately: they are the port's own
//     additions, not drift. A miner file that paired with a provider file —
//     under any rule, including a rename or a mapping — is a counterpart, not
//     an addition, so it is never listed there.
//
// Exit status is 0 when every gap is allowlisted, 1 when an unlisted gap
// exists, and 2 when the scan or its configuration cannot be read. The output
// is sorted and contains no paths or timestamps, so two runs on the same tree
// produce identical bytes and CI can diff it.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	exitOK    = 0
	exitGap   = 1
	exitError = 2
)

// fileDecls is one Go file's top-level surface. Only declarations without a
// receiver count: methods belong to their type and move with it.
type fileDecls struct {
	name     string
	stem     string
	funcs    []string
	types    []string
	names    map[string]bool
	exported map[string]bool
}

func (f *fileDecls) count() int { return len(f.funcs) + len(f.types) }

// has reports whether the file declares name under any of the variants the
// declaration matcher accepts. It is the per-file half of namePresent, so the
// pairing score cannot disagree with the declaration comparison about what
// counts as the same name.
func (f *fileDecls) has(name string) bool {
	for _, variant := range nameVariants(name) {
		if f.names[variant] {
			return true
		}
	}
	return false
}

// packageDecls is one package's files plus the name universe used to decide
// whether a declaration survived a move to another file.
type packageDecls struct {
	dir    string
	files  []*fileDecls
	byName map[string]*fileDecls
	funcs  map[string]bool
	types  map[string]bool
}

func scanPackage(dir string, includeTests bool) (*packageDecls, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	pkg := &packageDecls{
		byName: make(map[string]*fileDecls),
		funcs:  make(map[string]bool),
		types:  make(map[string]bool),
	}
	fset := token.NewFileSet()

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if !includeTests && strings.HasSuffix(name, "_test.go") {
			continue
		}

		path := filepath.Join(dir, name)
		parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		file := &fileDecls{
			name:     name,
			stem:     strings.TrimSuffix(name, ".go"),
			names:    make(map[string]bool),
			exported: make(map[string]bool),
		}
		add := func(declared string) {
			file.names[declared] = true
			if ast.IsExported(declared) {
				file.exported[declared] = true
			}
		}
		for _, decl := range parsed.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				// A method is not a top-level function; its owner type is
				// compared on its own.
				if decl.Recv != nil || decl.Name == nil {
					continue
				}
				file.funcs = append(file.funcs, decl.Name.Name)
				add(decl.Name.Name)
				pkg.funcs[decl.Name.Name] = true
			case *ast.GenDecl:
				if decl.Tok != token.TYPE {
					continue
				}
				for _, spec := range decl.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || typeSpec.Name == nil {
						continue
					}
					file.types = append(file.types, typeSpec.Name.Name)
					add(typeSpec.Name.Name)
					pkg.types[typeSpec.Name.Name] = true
				}
			}
		}

		sort.Strings(file.funcs)
		sort.Strings(file.types)
		pkg.files = append(pkg.files, file)
		pkg.byName[name] = file
	}

	sort.Slice(pkg.files, func(i, j int) bool { return pkg.files[i].name < pkg.files[j].name })
	return pkg, nil
}

// configEntry is one line of the allowlist or the mappings file. Both files
// share a shape so they stay easy to audit: "kind name ..." where the tail is
// a target for mappings and a mandatory one-line reason for the allowlist.
type configEntry struct {
	kind   string
	name   string
	target string
	reason string
}

func parseConfig(path string, allowlist bool) ([]configEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var entries []configEntry
	for lineNo, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		kind := fields[0]
		switch kind {
		case "file", "func", "type":
		default:
			return nil, fmt.Errorf("%s:%d: unknown kind %q (want file, func or type)", path, lineNo+1, kind)
		}

		if allowlist {
			// A bare name is not an allowlist entry: the reason is what keeps
			// the list honest after the gap has been forgotten.
			if len(fields) < 3 {
				return nil, fmt.Errorf("%s:%d: allowlist entry needs a reason: %s", path, lineNo+1, line)
			}
			entries = append(entries, configEntry{
				kind:   kind,
				name:   fields[1],
				reason: strings.Join(fields[2:], " "),
			})
			continue
		}

		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: mapping needs exactly: %s <provider> <miner>", path, lineNo+1, kind)
		}
		entries = append(entries, configEntry{kind: kind, name: fields[1], target: fields[2]})
	}
	return entries, nil
}

type options struct {
	root              string
	provider          string
	miner             string
	allowlist         string
	mappings          string
	includeTests      bool
	includeUnexported bool
	verbose           bool
	minerOnly         bool
}

type declGap struct {
	kind string
	name string
	file string
}

type report struct {
	providerFiles int
	providerFuncs int
	providerTypes int
	minerFiles    int
	minerFuncs    int
	minerTypes    int

	fileGaps  []*fileDecls
	declGaps  []declGap
	minerOnly []*fileDecls

	// pairings records the miner files each provider file was matched to, so
	// a split port is visible as a set rather than a single guessed target.
	pairings map[string][]string

	overlapMatched int
	multiFilePairs int

	allowlistedFiles int
	allowlistedFuncs int
	allowlistedTypes int
	allowlistEntries []configEntry

	gaps int
}

// nameVariants returns the names that count as the same declaration. Ports of
// this tree were made by replacing the package noun, and by dropping an
// export when a helper became package-private, so both are treated as moves
// rather than drift.
func nameVariants(name string) []string {
	seen := map[string]bool{}
	var variants []string
	add := func(value string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		variants = append(variants, value)
	}

	add(name)
	substituted := strings.ReplaceAll(name, "Provider", "Miner")
	substituted = strings.ReplaceAll(substituted, "provider", "miner")
	add(substituted)
	for _, value := range []string{name, substituted} {
		if value == "" {
			continue
		}
		add(strings.ToLower(value[:1]) + value[1:])
		add(strings.ToUpper(value[:1]) + value[1:])
	}
	return variants
}

func namePresent(name string, universe map[string]bool, mapping map[string]string) bool {
	if mapping != nil {
		if target, ok := mapping[name]; ok {
			// An explicit mapping is authoritative: a mapped declaration that
			// is absent is a gap, not a reason to fall back to the old name.
			return universe[target]
		}
	}
	for _, variant := range nameVariants(name) {
		if universe[variant] {
			return true
		}
	}
	return false
}

// fileCounterparts finds the miner files that carry a provider file's
// behaviour. Exact stems and explicit mappings are the common case, and a
// mapping may name several targets because a port can split one file in two.
//
// Renames and consolidations are caught by declaration coverage. Coverage is a
// union: a port that split a 1500-line provider file across five miner files
// cannot be recognised by any single file, so the counterpart is a *set* of
// miner files that jointly hold at least half of the provider file's
// declarations. When only the exported surface is compared, a set that holds
// every exported declaration is also enough — the unexported helpers it omits
// are not compared, and the README says they are expected to diverge.
//
// Membership is tested with namePresent's variants, so a port that lower-cased
// a type or swapped Provider for Miner still counts as the same declaration.
// Candidates are taken best-first (most declarations contributed, then the
// closest stem, then the tighter file) and only while they add something, so
// the chosen set is deterministic and minimal-ish; the order never decides
// whether a name exists, only which files are credited with it.
func fileCounterparts(providerFile *fileDecls, minerPkg *packageDecls, mappings map[string][]string, exportedOnly bool) ([]*fileDecls, string) {
	if targets, ok := mappings[providerFile.name]; ok {
		files := make([]*fileDecls, 0, len(targets))
		for _, target := range targets {
			if file := minerPkg.byName[target]; file != nil {
				files = append(files, file)
			}
		}
		return files, "mapping"
	}
	if same := minerPkg.byName[providerFile.name]; same != nil {
		return []*fileDecls{same}, "stem"
	}
	if providerFile.count() == 0 {
		return nil, ""
	}

	type candidate struct {
		file   *fileDecls
		shared int
		tokens int
		extra  int
	}
	candidates := make([]candidate, 0, len(minerPkg.files))
	for _, minerFile := range minerPkg.files {
		shared := 0
		for name := range providerFile.names {
			if minerFile.has(name) {
				shared++
			}
		}
		if shared == 0 {
			continue
		}
		candidates = append(candidates, candidate{
			file:   minerFile,
			shared: shared,
			tokens: stemTokenOverlap(providerFile.stem, minerFile.stem),
			extra:  minerFile.count() - shared,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		switch {
		case a.shared != b.shared:
			return a.shared > b.shared
		case a.tokens != b.tokens:
			return a.tokens > b.tokens
		case a.extra != b.extra:
			return a.extra < b.extra
		default:
			return a.file.name < b.file.name
		}
	})

	names := make([]string, 0, len(providerFile.names))
	for name := range providerFile.names {
		names = append(names, name)
	}
	sort.Strings(names)

	covered := map[string]bool{}
	picked := make([]*fileDecls, 0, 2)
	for _, candidate := range candidates {
		if coverageMet(providerFile, covered, exportedOnly) {
			break
		}
		added := false
		for _, name := range names {
			if covered[name] || !candidate.file.has(name) {
				continue
			}
			covered[name] = true
			added = true
		}
		if added {
			picked = append(picked, candidate.file)
		}
	}
	if !coverageMet(providerFile, covered, exportedOnly) {
		return nil, ""
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].name < picked[j].name })
	return picked, "overlap"
}

// coverageMet reports whether the declarations found so far are enough to call
// the provider file ported. Half the file is the rule; with the default
// exported-only comparison, a complete exported surface also qualifies.
func coverageMet(providerFile *fileDecls, covered map[string]bool, exportedOnly bool) bool {
	total := providerFile.count()
	if total == 0 {
		return false
	}
	if len(covered)*2 >= total {
		return true
	}
	if exportedOnly && len(providerFile.exported) > 0 {
		found := 0
		for name := range providerFile.exported {
			if covered[name] {
				found++
			}
		}
		return found == len(providerFile.exported)
	}
	return false
}

// stemTokenOverlap counts the underscore-separated words two stems share.
// "peercred_linux" and "control_socket_peercred_linux" share two; that is
// enough to prefer a renamed build-tag sibling over an unrelated file that
// happens to define the same one-line stub.
func stemTokenOverlap(a, b string) int {
	set := map[string]bool{}
	for _, token := range strings.Split(a, "_") {
		set[token] = true
	}
	shared := 0
	for _, token := range strings.Split(b, "_") {
		if set[token] {
			shared++
		}
	}
	return shared
}

func loadMapping(entries []configEntry, kind string) (map[string]string, error) {
	out := map[string]string{}
	for _, entry := range entries {
		if entry.kind != kind {
			continue
		}
		if out[entry.name] != "" {
			return nil, fmt.Errorf("duplicate mapping for %s %s", entry.kind, entry.name)
		}
		out[entry.name] = entry.target
	}
	return out, nil
}

// loadFileMapping keeps every target of a file mapping in file order. A split
// port needs several miner files, so repeating "file a.go b.go" and
// "file a.go c.go" is how a human says "this one became b.go and c.go".
// Repeating the same target is a harmless duplicate, not an error.
func loadFileMapping(entries []configEntry) map[string][]string {
	out := map[string][]string{}
	for _, entry := range entries {
		if entry.kind != "file" {
			continue
		}
		duplicate := false
		for _, target := range out[entry.name] {
			if target == entry.target {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out[entry.name] = append(out[entry.name], entry.target)
		}
	}
	return out
}

func loadConfigPath(root, path string, allowlist bool) ([]configEntry, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	return parseConfig(path, allowlist)
}

// validateDeclMappings checks that every func or type mapping resolves in the
// scan the comparison runs on. The mapping is authoritative — namePresent does
// not fall back to the provider name once one exists — so a mapping naming a
// declaration the scan does not hold either never applies (source side) or
// reports a ported declaration as a gap (target side). Both are configuration
// errors, not findings about the port.
//
// Only -tests puts _test.go declarations into those maps, so a mapping that
// names one is refused with a hint instead of claiming a symbol that is on disk
// does not exist. That is the same rule the file mappings follow.
func validateDeclMappings(kind string, mappings map[string]string, providerPkg, minerPkg *packageDecls, providerDir, minerDir string, includeTests bool) error {
	if len(mappings) == 0 {
		return nil
	}
	providerNames, minerNames := providerPkg.funcs, minerPkg.funcs
	if kind == "type" {
		providerNames, minerNames = providerPkg.types, minerPkg.types
	}

	names := make([]string, 0, len(mappings))
	for name := range mappings {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if !providerNames[name] {
			return missingDeclError("provider", kind, name, providerDir, includeTests)
		}
		target := mappings[name]
		if !minerNames[target] {
			return missingDeclError("miner", kind, target, minerDir, includeTests)
		}
	}
	return nil
}

// missingDeclError names a declaration a mapping could not resolve and, when
// the only reason is the scan's -tests setting, asks for that flag instead of
// claiming the declaration does not exist.
func missingDeclError(role, kind, name, dir string, includeTests bool) error {
	if !includeTests {
		if inTests, err := declaredInTestFiles(dir, kind, name); err == nil && inTests {
			return fmt.Errorf("%s %s %s is declared only in _test.go files, and only -tests scans those", role, kind, name)
		}
	}
	return fmt.Errorf("%s %s %s does not exist", role, kind, name)
}

// declaredInTestFiles reports whether a _test.go file in dir declares name as a
// top-level func (kind "func") or a type (kind "type"). The scan that built the
// lookup maps skipped those files, so it cannot answer this itself; the
// question only comes up for a mapping that did not resolve.
func declaredInTestFiles(dir, kind, name string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			return false, err
		}
		for _, decl := range parsed.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if kind == "func" && decl.Recv == nil && decl.Name != nil && decl.Name.Name == name {
					return true, nil
				}
			case *ast.GenDecl:
				if kind != "type" || decl.Tok != token.TYPE {
					continue
				}
				for _, spec := range decl.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if ok && typeSpec.Name != nil && typeSpec.Name.Name == name {
						return true, nil
					}
				}
			}
		}
	}
	return false, nil
}

func buildReport(opts options) (*report, error) {
	providerDir := filepath.Join(opts.root, opts.provider)
	minerDir := filepath.Join(opts.root, opts.miner)
	providerPkg, err := scanPackage(providerDir, opts.includeTests)
	if err != nil {
		return nil, err
	}
	minerPkg, err := scanPackage(minerDir, opts.includeTests)
	if err != nil {
		return nil, err
	}

	allowlistEntries, err := loadConfigPath(opts.root, opts.allowlist, true)
	if err != nil {
		return nil, err
	}
	mappingEntries, err := loadConfigPath(opts.root, opts.mappings, false)
	if err != nil {
		return nil, err
	}
	fileMappings := loadFileMapping(mappingEntries)
	funcMappings, err := loadMapping(mappingEntries, "func")
	if err != nil {
		return nil, err
	}
	typeMappings, err := loadMapping(mappingEntries, "type")
	if err != nil {
		return nil, err
	}
	mappingNames := make([]string, 0, len(fileMappings))
	for name := range fileMappings {
		mappingNames = append(mappingNames, name)
	}
	sort.Strings(mappingNames)
	// A run without -tests has no _test.go files in its scan, so a mapping that
	// names one must say so instead of claiming a file that is on disk is
	// missing. Only -tests changes what these lookups can see: -all affects
	// which declarations are compared, not which names exist.
	failMapped := func(role, file, context string) error {
		if !opts.includeTests && strings.HasSuffix(file, "_test.go") {
			return fmt.Errorf("%s: %s %s is a test file, and only -tests scans those", context, role, file)
		}
		return fmt.Errorf("%s: %s %s does not exist", context, role, file)
	}
	for _, name := range mappingNames {
		if providerPkg.byName[name] == nil {
			return nil, failMapped("provider file", name, "mapping "+name)
		}
		for _, target := range fileMappings[name] {
			if minerPkg.byName[target] == nil {
				return nil, failMapped("miner file", target, "mapping "+name+" -> "+target)
			}
		}
	}
	// Declaration mappings are validated against the same scan the comparison
	// uses, for the same reason and with the same -tests hint: a mapping is
	// authoritative, so one whose source does not exist silently never applies
	// and one whose target does not exist turns a ported declaration into a
	// false gap.
	if err := validateDeclMappings("func", funcMappings, providerPkg, minerPkg, providerDir, minerDir, opts.includeTests); err != nil {
		return nil, err
	}
	if err := validateDeclMappings("type", typeMappings, providerPkg, minerPkg, providerDir, minerDir, opts.includeTests); err != nil {
		return nil, err
	}

	allowedFile := map[string]string{}
	allowedName := map[string]string{}
	for _, entry := range allowlistEntries {
		switch entry.kind {
		case "file":
			allowedFile[entry.name] = entry.reason
		case "func", "type":
			allowedName[entry.kind+":"+entry.name] = entry.reason
		}
	}

	rep := &report{
		providerFiles: len(providerPkg.files),
		providerFuncs: len(providerPkg.funcs),
		providerTypes: len(providerPkg.types),
		minerFiles:    len(minerPkg.files),
		minerFuncs:    len(minerPkg.funcs),
		minerTypes:    len(minerPkg.types),
	}

	// Pair the files first: a file with no counterpart accounts for all of
	// its declarations, and reporting those again as name gaps would double
	// count the same missing behaviour. A counterpart is a set — a split port
	// has several — so the report records the whole set.
	paired := map[*fileDecls]bool{}
	// pairedMiner is the other half of the pairing. A counterpart keeps its
	// provider file's name only when the port kept the file name; a rename,
	// split or mapping pairs files whose names differ, so the miner-only
	// section must ask this set rather than compare names.
	pairedMiner := map[*fileDecls]bool{}
	rep.pairings = map[string][]string{}
	for _, providerFile := range providerPkg.files {
		minerFiles, how := fileCounterparts(providerFile, minerPkg, fileMappings, !opts.includeUnexported)
		if len(minerFiles) == 0 {
			if reason, ok := allowedFile[providerFile.name]; ok {
				rep.allowlistedFiles++
				rep.allowlistEntries = append(rep.allowlistEntries, configEntry{kind: "file", name: providerFile.name, reason: reason})
				continue
			}
			rep.fileGaps = append(rep.fileGaps, providerFile)
			continue
		}
		paired[providerFile] = true
		targets := make([]string, 0, len(minerFiles))
		for _, minerFile := range minerFiles {
			pairedMiner[minerFile] = true
			targets = append(targets, minerFile.name)
		}
		sort.Strings(targets)
		rep.pairings[providerFile.name] = targets
		if len(minerFiles) > 1 {
			rep.multiFilePairs++
		}
		if how == "overlap" {
			rep.overlapMatched++
		}
	}

	for _, providerFile := range providerPkg.files {
		if !paired[providerFile] {
			continue
		}
		// Since a file-level allowlist covers everything in that file, a
		// reason never has to be repeated for each of its helpers.
		if _, ok := allowedFile[providerFile.name]; ok {
			continue
		}

		for _, name := range providerFile.funcs {
			if !opts.includeUnexported && !ast.IsExported(name) {
				continue
			}
			if namePresent(name, minerPkg.funcs, funcMappings) {
				continue
			}
			if reason, ok := allowedName["func:"+name]; ok {
				rep.allowlistedFuncs++
				rep.allowlistEntries = append(rep.allowlistEntries, configEntry{kind: "func", name: name, reason: reason})
				continue
			}
			rep.declGaps = append(rep.declGaps, declGap{kind: "func", name: name, file: providerFile.name})
		}
		for _, name := range providerFile.types {
			if !opts.includeUnexported && !ast.IsExported(name) {
				continue
			}
			if namePresent(name, minerPkg.types, typeMappings) {
				continue
			}
			if reason, ok := allowedName["type:"+name]; ok {
				rep.allowlistedTypes++
				rep.allowlistEntries = append(rep.allowlistEntries, configEntry{kind: "type", name: name, reason: reason})
				continue
			}
			rep.declGaps = append(rep.declGaps, declGap{kind: "type", name: name, file: providerFile.name})
		}
	}

	// The port's own additions are informational. They never fail the run;
	// they are here so a reviewer can tell a new miner behaviour from a
	// provider behaviour that failed to cross over. Membership in pairedMiner
	// is the test, not the file name: a renamed, split or mapped counterpart
	// carries provider behaviour and must not read as an addition.
	for _, file := range minerPkg.files {
		if !pairedMiner[file] {
			rep.minerOnly = append(rep.minerOnly, file)
		}
	}

	sort.Slice(rep.declGaps, func(i, j int) bool {
		if rep.declGaps[i].name != rep.declGaps[j].name {
			return rep.declGaps[i].name < rep.declGaps[j].name
		}
		return rep.declGaps[i].file < rep.declGaps[j].file
	})
	sort.Slice(rep.allowlistEntries, func(i, j int) bool {
		if rep.allowlistEntries[i].kind != rep.allowlistEntries[j].kind {
			return rep.allowlistEntries[i].kind < rep.allowlistEntries[j].kind
		}
		return rep.allowlistEntries[i].name < rep.allowlistEntries[j].name
	})
	rep.gaps = len(rep.fileGaps) + len(rep.declGaps)
	return rep, nil
}

func declCount(n int) string {
	if n == 1 {
		return "1 decl"
	}
	return fmt.Sprintf("%d decls", n)
}

func (rep *report) render(opts options) string {
	var out strings.Builder
	fmt.Fprintf(&out, "provider -> miner parity check\n")
	fmt.Fprintf(&out, "  provider: %d files, %d funcs, %d types\n", rep.providerFiles, rep.providerFuncs, rep.providerTypes)
	fmt.Fprintf(&out, "  miner:    %d files, %d funcs, %d types\n", rep.minerFiles, rep.minerFuncs, rep.minerTypes)

	fmt.Fprintf(&out, "\n(a) provider files with no miner counterpart (%d)\n", len(rep.fileGaps))
	for _, file := range rep.fileGaps {
		fmt.Fprintf(&out, "  %-44s %s\n", file.name, declCount(file.count()))
	}

	fmt.Fprintf(&out, "\n(b) provider declarations with no miner counterpart (%d)\n", len(rep.declGaps))
	for _, gap := range rep.declGaps {
		fmt.Fprintf(&out, "  %-5s %-38s %s\n", gap.kind, gap.name, gap.file)
	}

	fmt.Fprintf(&out, "\nallowlisted: %d file(s), %d func(s), %d type(s)\n",
		rep.allowlistedFiles, rep.allowlistedFuncs, rep.allowlistedTypes)
	if opts.verbose {
		for _, entry := range rep.allowlistEntries {
			fmt.Fprintf(&out, "  %-4s %-38s %s\n", entry.kind, entry.name, entry.reason)
		}
	}
	fmt.Fprintf(&out, "matched by declaration overlap: %d file(s)\n", rep.overlapMatched)
	if rep.multiFilePairs > 0 {
		fmt.Fprintf(&out, "paired with more than one miner file: %d file(s)\n", rep.multiFilePairs)
	}
	if opts.verbose {
		names := make([]string, 0, len(rep.pairings))
		for name := range rep.pairings {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(&out, "file pairings:\n")
		for _, name := range names {
			fmt.Fprintf(&out, "  %-44s %s\n", name, strings.Join(rep.pairings[name], " "))
		}
	}

	if rep.gaps > 0 {
		fmt.Fprintf(&out, "\nFAIL: %d unlisted gap(s)\n", rep.gaps)
	} else {
		fmt.Fprintf(&out, "\nOK: no unlisted gaps\n")
	}

	if opts.minerOnly {
		fmt.Fprintf(&out, "\n(c) miner-only files - port additions, informational (%d)\n", len(rep.minerOnly))
		for _, file := range rep.minerOnly {
			fmt.Fprintf(&out, "  %s\n", file.name)
		}
	}
	return out.String()
}

func run(opts options) (string, int, error) {
	rep, err := buildReport(opts)
	if err != nil {
		return "", exitError, err
	}
	code := exitOK
	if rep.gaps > 0 {
		code = exitGap
	}
	return rep.render(opts), code, nil
}

func main() {
	opts := options{}
	flag.StringVar(&opts.root, "root", ".", "repository root")
	flag.StringVar(&opts.provider, "provider", "provider", "provider package directory, relative to -root")
	flag.StringVar(&opts.miner, "miner", "miner", "miner package directory, relative to -root")
	flag.StringVar(&opts.allowlist, "allowlist", "scripts/parity-check/allowlist.txt", "known-intentional gaps, relative to -root")
	flag.StringVar(&opts.mappings, "mappings", "scripts/parity-check/mappings.txt", "explicit rename map, relative to -root")
	flag.BoolVar(&opts.includeTests, "tests", false, "include _test.go files")
	flag.BoolVar(&opts.includeUnexported, "all", false, "also compare unexported declarations")
	flag.BoolVar(&opts.verbose, "v", false, "explain allowlist entries")
	flag.BoolVar(&opts.minerOnly, "miner-only", true, "list miner-only files (informational)")
	flag.Parse()

	if flag.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "parity-check: unexpected arguments: %s\n", strings.Join(flag.Args(), " "))
		os.Exit(exitError)
	}

	output, code, err := run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parity-check: %v\n", err)
		os.Exit(exitError)
	}
	fmt.Print(output)
	os.Exit(code)
}
