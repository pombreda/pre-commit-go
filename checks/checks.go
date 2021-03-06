// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// Package checks defines pre-made checks and custom checks for pre-commit-go.
//
// Each of the struct in this files will be embedded into pre-commit-go.yml.
// Use the comments here as a guidance to set the relevant values.
package checks

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// CheckPrerequisite describe a Go package that is needed to run a Check.
//
// It must list a command that is to be executed and the expected exit code to
// verify that the custom tool is properly installed. If the executable is not
// detected, "go get $URL" will be executed.
type CheckPrerequisite struct {
	HelpCommand      []string
	ExpectedExitCode int
	URL              string
}

// Check describes an check to be executed on the code base.
type Check interface {
	// GetRunLevel is the level at which this check should be run.
	GetRunLevel() int
	// GetMaxDuration is the maximum of seconds allowed to run this check.
	GetMaxDuration() int
	// GetDescription returns the check description.
	GetDescription() string
	// GetName returns the check name.
	GetName() string
	// GetPrerequisites lists all the go packages to be installed before running
	// this check.
	GetPrerequisites() []CheckPrerequisite
	// ResetDefault resets the check to its default values.
	ResetDefault()
	// Run executes the check.
	Run() error
}

// CheckCommon defines the common properties of each check to be serialized in
// the configuration file.
type CheckCommon struct {
	// [0, 3]. 0 is never, 3 is always. Default:
	//   - most checks that only require the stdlib have default RunLevel of 1
	//   - most checks that require third parties have default RunLevel of 2
	//   - checks that may trigger false positives have default RunLevel of 3
	RunLevel int
	// In seconds. Default to MaxDuration at global scope. The value is omitted
	// by default since it's likely to be 0 everywhere most of the time.
	MaxDuration int `yaml:",omitempty";`
}

func (c *CheckCommon) getRunLevel() int {
	return c.RunLevel
}

func (c *CheckCommon) getMaxDuration() int {
	return c.MaxDuration
}

// check exists to reduce the noise in the doc.
type check interface {
	getRunLevel() int
	getMaxDuration() int
	getDescription() string
	getName() string
	getPrerequisites() []CheckPrerequisite
	resetDefault()
	run() error
}

type checkAdaptor struct {
	check
}

func (c checkAdaptor) GetRunLevel() int {
	return c.getRunLevel()
}
func (c checkAdaptor) GetMaxDuration() int {
	return c.getMaxDuration()
}
func (c checkAdaptor) GetDescription() string {
	return c.getDescription()
}
func (c checkAdaptor) GetName() string {
	return c.getName()
}
func (c checkAdaptor) GetPrerequisites() []CheckPrerequisite {
	return c.getPrerequisites()
}
func (c checkAdaptor) ResetDefault() {
	c.resetDefault()
}
func (c checkAdaptor) Run() error {
	return c.run()
}

// Native checks.

// BuildOnly builds everything inside the current directory via
// 'go build ./...'.
//
// This check is mostly useful for executables, that is, "package main".
// Packages containing tests are covered via check Test.
type BuildOnly struct {
	CheckCommon `yaml:",inline"`
	// Default is empty. Can be used to build multiple times with different
	// tags, e.g. to build -tags foo,zoo then -tags bar.
	ExtraArgs [][]string
}

func (b *BuildOnly) Check() Check {
	return checkAdaptor{b}
}

func (b *BuildOnly) getDescription() string {
	return "builds all packages that do not contain tests, usually all directories with package 'main'"
}

func (b *BuildOnly) getName() string {
	return "build"
}

func (b *BuildOnly) getPrerequisites() []CheckPrerequisite {
	return nil
}

func (b *BuildOnly) resetDefault() {
	b.RunLevel = 1
	b.MaxDuration = 0
	b.ExtraArgs = [][]string{{}}
}

func (b *BuildOnly) run() error {
	if len(b.ExtraArgs) == 0 {
		return fmt.Errorf("ExtraArgs must be at least a list of one empty list")
	}
	// Cannot build concurrently since it leaves files in the tree.
	// TODO(maruel): Build in a temporary directory to not leave junk in the tree
	// with -o. On the other hand, ./... and -o foo are incompatible. But
	// building would have to be done in an efficient way by looking at which
	// package builds what, to not result in a O(n²) algorithm.
	for _, extraarg := range b.ExtraArgs {
		args := []string{"go", "build"}
		args = append(args, extraarg...)
		args = append(args, "./...")
		out, _, err := capture(args...)
		if len(out) != 0 {
			return fmt.Errorf("%s failed: %s", strings.Join(args, " "), out)
		}
		if err != nil {
			return fmt.Errorf("%s failed: %s", strings.Join(args, " "), err.Error())
		}
	}
	return nil
}

// Gofmt runs gofmt in check mode with code simplification enabled.
//
// It is almost redundant with goimports except for '-s' which goimports
// doesn't implement and gofmt doesn't require any external package.
type Gofmt struct {
	CheckCommon `yaml:",inline"`
}

func (g *Gofmt) Check() Check {
	return checkAdaptor{g}
}

func (g *Gofmt) getDescription() string {
	return "enforces all .go sources are formatted with 'gofmt -s'"
}

func (g *Gofmt) getName() string {
	return "gofmt"
}

func (g *Gofmt) getPrerequisites() []CheckPrerequisite {
	return nil
}

func (g *Gofmt) resetDefault() {
	g.RunLevel = 1
	g.MaxDuration = 0
}

func (g *Gofmt) run() error {
	// gofmt doesn't return non-zero even if some files need to be updated.
	out, _, err := capture("gofmt", "-l", "-s", ".")
	if len(out) != 0 {
		return fmt.Errorf("these files are improperly formmatted, please run: gofmt -w -s .\n%s", out)
	}
	if err != nil {
		return fmt.Errorf("gofmt -l -s . failed: %s", err)
	}
	return nil
}

// Test runs all tests via go test.
//
// It is possible to run all tests multiple times, for example if one want to
// use -tags. Note that TestCoverage is generally a better choice, the main
// exception is the use of -race.
type Test struct {
	CheckCommon `yaml:",inline"`
	// Default is -v -race. Additional arguments to pass, like -race. Can be used
	// multiple times to run tests multiple times, for example with -tags.
	ExtraArgs [][]string
}

func (t *Test) Check() Check {
	return checkAdaptor{t}
}

func (t *Test) getDescription() string {
	return "runs all tests, potentially multiple times (with race detector, with different tags, etc)"
}

func (t *Test) getName() string {
	return "test"
}

func (t *Test) getPrerequisites() []CheckPrerequisite {
	return nil
}

func (t *Test) resetDefault() {
	t.RunLevel = 1
	t.MaxDuration = 0
	t.ExtraArgs = [][]string{{"-v", "-race"}}
}

func (t *Test) run() error {
	if len(t.ExtraArgs) == 0 {
		return fmt.Errorf("ExtraArgs must be at least a list of one empty list")
	}
	// Add tests manually instead of using './...'. The reason is that it permits
	// running all the tests concurrently, which saves a lot of time when there's
	// many packages.
	var wg sync.WaitGroup
	testDirs := goDirs(true)
	for _, extraarg := range t.ExtraArgs {
		errs := make(chan error, len(testDirs))
		for _, td := range testDirs {
			wg.Add(1)
			go func(testDir string, extraarg []string) {
				defer wg.Done()
				rel, err := relToGOPATH(testDir)
				if err != nil {
					errs <- err
					return
				}
				args := []string{"go", "test"}
				args = append(args, extraarg...)
				args = append(args, rel)
				out, exitCode, _ := capture(args...)
				if exitCode != 0 {
					errs <- fmt.Errorf("%s failed:\n%s", strings.Join(args, " "), out)
				}
			}(td, extraarg)
		}
		wg.Wait()
		select {
		case err := <-errs:
			return err
		default:
		}
	}
	return nil
}

// Non-native checks; running these require installing third party packages. As
// such, they are by default at an higher run level.

// Errcheck runs errcheck on all directories containing .go files.
type Errcheck struct {
	CheckCommon `yaml:",inline"`
	// Flag to pass to -ignore. Default is "Close".
	Ignores string
}

func (e *Errcheck) Check() Check {
	return checkAdaptor{e}
}

func (e *Errcheck) getDescription() string {
	return "enforces all calls returning an error are checked using tool 'errcheck'"
}

func (e *Errcheck) getName() string {
	return "errcheck"
}

func (e *Errcheck) getPrerequisites() []CheckPrerequisite {
	return []CheckPrerequisite{
		{[]string{"errcheck", "-h"}, 2, "github.com/kisielk/errcheck"},
	}
}

func (e *Errcheck) resetDefault() {
	e.RunLevel = 2
	e.MaxDuration = 0
	// "Close|Write.*|Flush|Seek|Read.*"
	e.Ignores = "Close"
}

func (e *Errcheck) run() error {
	dirs := goDirs(false)
	args := make([]string, 0, len(dirs)+2)
	args = append(args, "errcheck", "-ignore", e.Ignores)
	for _, d := range dirs {
		rel, err := relToGOPATH(d)
		if err != nil {
			return err
		}
		args = append(args, rel)
	}
	out, _, err := capture(args...)
	if len(out) != 0 {
		return fmt.Errorf("%s failed:\n%s", strings.Join(args, " "), out)
	}
	if err != nil {
		return fmt.Errorf("%s failed: %s", strings.Join(args, " "), err)
	}
	return nil
}

// Goimports runs goimports in check mode.
type Goimports struct {
	CheckCommon `yaml:",inline"`
}

func (g *Goimports) Check() Check {
	return checkAdaptor{g}
}

func (g *Goimports) getDescription() string {
	return "enforces all .go sources are formatted with 'goimports'"
}

func (g *Goimports) getName() string {
	return "goimports"
}

func (g *Goimports) getPrerequisites() []CheckPrerequisite {
	return []CheckPrerequisite{
		{[]string{"goimports", "-h"}, 2, "golang.org/x/tools/cmd/goimports"},
	}
}

func (g *Goimports) resetDefault() {
	g.RunLevel = 2
	g.MaxDuration = 0
}

func (g *Goimports) run() error {
	// goimports doesn't return non-zero even if some files need to be updated.
	out, _, err := capture("goimports", "-l", ".")
	if len(out) != 0 {
		return fmt.Errorf("these files are improperly formmatted, please run: goimports -w .\n%s", out)
	}
	if err != nil {
		return fmt.Errorf("goimports -w . failed: %s", err)
	}
	return nil
}

// Golint runs golint.
//
// golint triggers false positives by design. Use Blacklist to ignore
// messages wholesale.
type Golint struct {
	CheckCommon `yaml:",inline"`
	// Messages generated by golint to be ignored.
	Blacklist []string
}

func (g *Golint) Check() Check {
	return checkAdaptor{g}
}

func (g *Golint) getDescription() string {
	return "enforces all .go sources passes golint"
}

func (g *Golint) getName() string {
	return "golint"
}

func (g *Golint) getPrerequisites() []CheckPrerequisite {
	return []CheckPrerequisite{
		{[]string{"golint", "-h"}, 2, "github.com/golang/lint/golint"},
	}
}

func (g *Golint) resetDefault() {
	g.RunLevel = 3
	g.MaxDuration = 0
	g.Blacklist = []string{}
}

func (g *Golint) run() error {
	// golint doesn't return non-zero ever.
	out, _, _ := capture("golint", "./...")
	result := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		for _, b := range g.Blacklist {
			if strings.Contains(line, b) {
				continue
			}
		}
		result = append(result, line)
	}
	if len(result) == 0 {
		return errors.New(strings.Join(result, "\n"))
	}
	return nil
}

// Govet runs "go tool vet".
//
// govet triggers false positives by design. Use Blacklist to ignore
// messages wholesale.
type Govet struct {
	CheckCommon `yaml:",inline"`
	// Messages generated by go tool vet to be ignored.
	Blacklist []string
}

func (g *Govet) Check() Check {
	return checkAdaptor{g}
}

func (g *Govet) getDescription() string {
	return "enforces all .go sources passes go tool vet"
}

func (g *Govet) getName() string {
	return "govet"
}

func (g *Govet) getPrerequisites() []CheckPrerequisite {
	return []CheckPrerequisite{
		{[]string{"go", "tool", "vet", "-h"}, 1, "golang.org/x/tools/cmd/vet"},
	}
}

func (g *Govet) resetDefault() {
	g.RunLevel = 3
	g.MaxDuration = 0
	g.Blacklist = []string{" composite literal uses unkeyed fields"}
}

func (g *Govet) run() error {
	// Ignore the return code since we ignore many errors.
	out, _, _ := capture("go", "tool", "vet", "-all", ".")
	result := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		for _, b := range g.Blacklist {
			if strings.Contains(line, b) {
				continue
			}
		}
		result = append(result, line)
	}
	if len(result) == 0 {
		return errors.New(strings.Join(result, "\n"))
	}
	return nil
}

// TestCoverage runs all tests with coverage.
//
// Each testable package is run with 'go test -cover' then all coverage
// information is merged together. This means that package X/Y may create code
// coverage for package X/Z.
//
// When running on https://travis-ci.org, it tries to upload code coverage
// results to https://coveralls.io.
//
// Otherwise, only a summary is printed in case code coverage is not above
// t.MinimumCoverage.
type TestCoverage struct {
	CheckCommon `yaml:",inline"`
	// Minimum test coverage to be generated or the check is considered to fail.
	MinimumCoverage float64
}

func (t *TestCoverage) Check() Check {
	return checkAdaptor{t}
}

func (t *TestCoverage) getDescription() string {
	return "enforces minimum test coverage on all packages that are not 'main'"
}

func (t *TestCoverage) getName() string {
	return "testcoverage"
}

func (t *TestCoverage) getPrerequisites() []CheckPrerequisite {
	toInstall := []CheckPrerequisite{
		{[]string{"go", "tool", "cover", "-h"}, 1, "golang.org/x/tools/cmd/cover"},
	}
	if len(os.Getenv("TRAVIS_JOB_ID")) != 0 {
		toInstall = append(toInstall, CheckPrerequisite{[]string{"goveralls", "-h"}, 2, "github.com/mattn/goveralls"})
	}
	return toInstall
}

func (t *TestCoverage) resetDefault() {
	t.RunLevel = 2
	t.MaxDuration = 0
	t.MinimumCoverage = 20.
}

func (t *TestCoverage) run() (err error) {
	pkgRoot, _ := os.Getwd()
	pkg, err2 := relToGOPATH(pkgRoot)
	if err2 != nil {
		return err2
	}
	testDirs := goDirs(true)
	if len(testDirs) == 0 {
		return nil
	}

	tmpDir, err2 := ioutil.TempDir("", "pre-commit-go")
	if err2 != nil {
		return err2
	}
	defer func() {
		err2 := os.RemoveAll(tmpDir)
		if err == nil {
			err = err2
		}
	}()

	// This part is similar to Test.Run() except that it passes a unique
	// -coverprofile file name, so that all the files can later be merged into a
	// single file.
	var wg sync.WaitGroup
	errs := make(chan error, len(testDirs))
	for i, td := range testDirs {
		wg.Add(1)
		go func(index int, testDir string) {
			defer wg.Done()
			args := []string{
				"go", "test", "-v", "-covermode=count", "-coverpkg", pkg + "/...",
				"-coverprofile", filepath.Join(tmpDir, fmt.Sprintf("test%d.cov", index)),
			}
			out, exitCode, _ := captureWd(testDir, args...)
			if exitCode != 0 {
				errs <- fmt.Errorf("%s %s failed:\n%s", strings.Join(args, " "), testDir, out)
			}
		}(i, td)
	}
	wg.Wait()

	// Merge the profiles. Sums all the counts.
	// Format is "file.go:XX.YY,ZZ.II J K"
	// J is number of statements, K is count.
	files, err2 := filepath.Glob(filepath.Join(tmpDir, "test*.cov"))
	if err2 != nil {
		return err2
	}
	if len(files) == 0 {
		select {
		case err2 := <-errs:
			return err2
		default:
			return errors.New("no coverage found")
		}
	}
	counts := map[string]int{}
	for _, file := range files {
		f, err2 := os.Open(file)
		if err2 != nil {
			return err2
		}
		s := bufio.NewScanner(f)
		// Strip the first line.
		s.Scan()
		count := 0
		for s.Scan() {
			items := rsplitn(s.Text(), " ", 2)
			count, err2 = strconv.Atoi(items[1])
			if err2 != nil {
				break
			}
			counts[items[0]] += int(count)
		}
		f.Close()
		if err2 != nil {
			return err2
		}
	}
	profilePath := filepath.Join(tmpDir, "profile.cov")
	f, err2 := os.Create(profilePath)
	if err2 != nil {
		return err2
	}
	stms := make([]string, 0, len(counts))
	for k := range counts {
		stms = append(stms, k)
	}
	sort.Strings(stms)
	_, _ = io.WriteString(f, "mode: count\n")
	for _, stm := range stms {
		fmt.Fprintf(f, "%s %d\n", stm, counts[stm])
	}
	f.Close()

	// Analyze the results.
	out, _, err2 := capture("go", "tool", "cover", "-func", profilePath)
	type fn struct {
		loc  string
		name string
	}
	coverage := map[fn]float64{}
	var total float64
	for i, line := range strings.Split(out, "\n") {
		if i == 0 || len(line) == 0 {
			// First or last line.
			continue
		}
		items := strings.SplitN(line, "\t", 2)
		loc := items[0]
		if len(items) == 1 {
			panic(fmt.Sprintf("%#v %#v", line, items))
		}
		items = strings.SplitN(strings.TrimLeft(items[1], "\t"), "\t", 2)
		name := items[0]
		percentStr := strings.TrimLeft(items[1], "\t")
		percent, err2 := strconv.ParseFloat(percentStr[:len(percentStr)-1], 64)
		if err2 != nil {
			return fmt.Errorf("malformed coverage file")
		}
		if loc == "total:" {
			total = percent
		} else {
			coverage[fn{loc, name}] = percent
		}
	}
	if total < t.MinimumCoverage {
		partial := 0
		for _, percent := range coverage {
			if percent < 100. {
				partial++
			}
		}
		err2 = fmt.Errorf("code coverage: %3.1f%%; %d untested functions", total, partial)
	}
	if err2 == nil {
		select {
		case err2 = <-errs:
		default:
		}
	}

	// Sends to coveralls.io if applicable.
	if len(os.Getenv("TRAVIS_JOB_ID")) != 0 {
		// Make sure to have registered to https://coveralls.io first!
		out, _, err3 := capture("goveralls", "-coverprofile", profilePath)
		fmt.Printf("%s", out)
		if err2 == nil {
			err2 = err3
		}
	}
	return err2
}

// Extensibility.

// CustomCheck represents a user configured check.
type CustomCheck struct {
	CheckCommon `yaml:",inline"`
	// Check's display name, required.
	Name string
	// Check's description, optional.
	Description string
	// Check's command line, required.
	Command []string
	// Check's fails if exit code is non-zero.
	CheckExitCode bool
	// Check's prerequisite packages to install first before running the check,
	// optional.
	Prerequisites []CheckPrerequisite
}

func (c *CustomCheck) Check() Check {
	return checkAdaptor{c}
}

func (c *CustomCheck) getDescription() string {
	return c.Description
}

func (c *CustomCheck) getName() string {
	return c.Name
}

func (c *CustomCheck) getPrerequisites() []CheckPrerequisite {
	return c.Prerequisites
}

func (c *CustomCheck) resetDefault() {
	// There's no default for a custom check.
}

func (c *CustomCheck) run() error {
	out, exitCode, err := capture(c.Command...)
	if exitCode != 0 && c.CheckExitCode {
		return fmt.Errorf("%d failed:\n%s", strings.Join(c.Command, " "), out)
	}
	return err
}
