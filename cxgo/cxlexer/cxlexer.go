package cxlexer

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"

	cxcore "github.com/SkycoinProject/cx/cx"
	"github.com/SkycoinProject/cx/cxgo/actions"
	"github.com/SkycoinProject/cx/cxgo/cxgo0"
	"github.com/SkycoinProject/cx/cxgo/cxprof"
	"github.com/SkycoinProject/cx/cxgo/parser"
)

// re contains all regular expressions used for lexing
var re = struct {
	// comments
	comment      *regexp.Regexp
	multiCommentOpen *regexp.Regexp
	multiCommentClose *regexp.Regexp

	// packages and structs
	pkg *regexp.Regexp
	pkgName *regexp.Regexp
	str *regexp.Regexp
	strName *regexp.Regexp

	// globals
	gl     *regexp.Regexp
	glName *regexp.Regexp

	// body open/close
	bodyOpen  *regexp.Regexp
	bodyClose *regexp.Regexp

	// imports
	imp *regexp.Regexp
	impName *regexp.Regexp
}{
	comment:           regexp.MustCompile("//"),
	multiCommentOpen:  regexp.MustCompile(`/\*`),
	multiCommentClose: regexp.MustCompile(`\*/`),

	pkg:               regexp.MustCompile("package"),
	pkgName:           regexp.MustCompile("(^|[\\s])package\\s+([_a-zA-Z][_a-zA-Z0-9]*)"),
	str:               regexp.MustCompile("type"),
	strName:           regexp.MustCompile("(^|[\\s])type\\s+([_a-zA-Z][_a-zA-Z0-9]*)?\\s"),

	gl:                regexp.MustCompile("var"),
	glName:            regexp.MustCompile("(^|[\\s])var\\s([_a-zA-Z][_a-zA-Z0-9]*)"),

	bodyOpen:          regexp.MustCompile("{"),
	bodyClose:         regexp.MustCompile("}"),

	imp:               regexp.MustCompile("import"),
	impName:           regexp.MustCompile("(^|[\\s])import\\s+\"([_a-zA-Z][_a-zA-Z0-9/-]*)\""),
}

// lg contains loggers
var lg = struct {
	l1 logrus.FieldLogger // packages and structs
	l2 logrus.FieldLogger // globals
	l3 logrus.FieldLogger // cxgo0
	l4 logrus.FieldLogger // parse
}{}

func SetLogger(log logrus.FieldLogger) {
	if log != nil {
		lg.l1 = log.WithField("i", 1).WithField("section", "packages/structs")
		lg.l2 = log.WithField("i", 2).WithField("section", "globals")
		lg.l3 = log.WithField("i", 3).WithField("section", "cxgo0")
		lg.l4 = log.WithField("i", 4).WithField("section", "parse")
	}
}

// Step0 performs a first pass for the CX parser. Globals, packages and
// custom types are added to `cxgo0.PRGRM0`.
func Step0(srcStrs, srcNames []string) int {
	var prePkg *cxcore.CXPackage
	parseErrors := 0

	_, stopL1 := cxprof.StartProfile(lg.l1)
	// 1. Identify all the packages and structs
	for srcI, srcStr := range srcStrs {
		srcName := srcNames[srcI]
		_, stopL1x := cxprof.StartProfile(lg.l1.WithField("src_file", srcName))

		reader := strings.NewReader(srcStr)
		scanner := bufio.NewScanner(reader)
		var inComment bool
		var lineN = 0

		for scanner.Scan() {
			line := scanner.Bytes()
			lineN++

			cl := makeCommentLocator(line)

			if skip := cl.skipLine(&inComment, true); skip {
				continue
			}

			// At this point we know that we are *not* in a comment

			// 1a. Identify all the packages
			if loc := re.pkg.FindIndex(line); loc != nil {
				if (cl.singleOpen != nil && cl.singleOpen[0] < loc[0]) ||
					(cl.multiOpen != nil && cl.multiOpen[0] < loc[0]) ||
					(cl.multiClose != nil && cl.multiClose[0] > loc[0]) {
					// then it's commented out
					continue
				}

				if match := re.pkgName.FindStringSubmatch(string(line)); match != nil {
					if pkg, err := cxgo0.PRGRM0.GetPackage(match[len(match)-1]); err != nil {
						// then it hasn't been added
						newPkg := cxcore.MakePackage(match[len(match)-1])
						cxgo0.PRGRM0.AddPackage(newPkg)
						prePkg = newPkg
					} else {
						prePkg = pkg
					}
				}
			}

			// 1b. Identify all the structs
			if loc := re.str.FindIndex(line); loc != nil {
				if (cl.singleOpen != nil && cl.singleOpen[0] < loc[0]) ||
					(cl.multiOpen != nil && cl.multiOpen[0] < loc[0]) ||
					(cl.multiClose != nil && cl.multiClose[0] > loc[0]) {
					// then it's commented out
					continue
				}

				if match := re.strName.FindStringSubmatch(string(line)); match != nil {
					if prePkg == nil {
						println(cxcore.CompilationError(srcName, lineN), "No package defined")
					} else if _, err := cxgo0.PRGRM0.GetStruct(match[len(match)-1], prePkg.Name); err != nil {
						// then it hasn't been added
						strct := cxcore.MakeStruct(match[len(match)-1])
						prePkg.AddStruct(strct)
					}
				}
			}
		}
		stopL1x()
	} // for range srcStrs
	stopL1()

	_, stopL2 := cxprof.StartProfile(lg.l2)
	// 2. Identify all global variables
	//    We also identify packages again, so we know to what
	//    package we're going to add the variable declaration to.
	for _, source := range srcStrs {
		_, stopL2x := cxprof.StartProfile(lg.l2.WithField("src_file", source))

		// inBlock needs to be 0 to guarantee that we're in the global scope
		var inBlock int
		var commentedCode bool

		s := bufio.NewScanner(strings.NewReader(source))
		for s.Scan() {
			line := s.Bytes()

			// we need to ignore function bodies
			// it'll also ignore struct declaration's bodies, but this doesn't matter
			commentLoc := re.comment.FindIndex(line)

			multiCommentOpenLoc := re.multiCommentOpen.FindIndex(line)
			multiCommentCloseLoc := re.multiCommentClose.FindIndex(line)

			if commentedCode && multiCommentCloseLoc != nil {
				commentedCode = false
			}

			if commentedCode {
				continue
			}

			if multiCommentOpenLoc != nil && !commentedCode && multiCommentCloseLoc == nil {
				commentedCode = true
				// continue
			}

			// Identify all the package imports.
			if loc := re.imp.FindIndex(line); loc != nil {
				if (commentLoc != nil && commentLoc[0] < loc[0]) ||
					(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] < loc[0]) ||
					(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
					// then it's commented out
					continue
				}

				if match := re.impName.FindStringSubmatch(string(line)); match != nil {
					pkgName := match[len(match)-1]
					// Checking if `pkgName` already exists and if it's not a standard library package.
					if _, err := cxgo0.PRGRM0.GetPackage(pkgName); err != nil && !cxcore.IsCorePackage(pkgName) {
						// _, sourceCode, srcNames := ParseArgsForCX([]string{fmt.Sprintf("%s%s", SRCPATH, pkgName)}, false)
						_, sourceCode, fileNames := cxcore.ParseArgsForCX([]string{filepath.Join(cxcore.SRCPATH, pkgName)}, false)
						ParseSourceCode(sourceCode, fileNames) // TODO @evanlinjin: Check return value.
					}
				}
			}

			// we search for packages at the same time, so we can know to what package to add the global
			if loc := re.pkg.FindIndex(line); loc != nil {
				if (commentLoc != nil && commentLoc[0] < loc[0]) ||
					(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] < loc[0]) ||
					(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
					// then it's commented out
					continue
				}

				if match := re.pkgName.FindStringSubmatch(string(line)); match != nil {
					if pkg, err := cxgo0.PRGRM0.GetPackage(match[len(match)-1]); err != nil {
						// then it hasn't been added
						prePkg = cxcore.MakePackage(match[len(match)-1])
						cxgo0.PRGRM0.AddPackage(prePkg)
					} else {
						prePkg = pkg
					}
				}
			}

			if locs := re.bodyOpen.FindAllIndex(line, -1); locs != nil {
				for _, loc := range locs {
					if !(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
						// then it's outside of a */, e.g. `*/ }`
						if (commentLoc == nil && multiCommentOpenLoc == nil && multiCommentCloseLoc == nil) ||
							(commentLoc != nil && commentLoc[0] > loc[0]) ||
							(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] > loc[0]) ||
							(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] < loc[0]) {
							// then we have an uncommented opening bracket
							inBlock++
						}
					}
				}
			}

			if locs := re.bodyClose.FindAllIndex(line, -1); locs != nil {
				for _, loc := range locs {
					if !(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
						if (commentLoc == nil && multiCommentOpenLoc == nil && multiCommentCloseLoc == nil) ||
							(commentLoc != nil && commentLoc[0] > loc[0]) ||
							(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] > loc[0]) ||
							(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] < loc[0]) {
							// then we have an uncommented closing bracket
							inBlock--
						}
					}
				}
			}

			// we could have this situation: {var local i32}
			// but we don't care about this, as the later passes will throw an error as it's invalid syntax

			if loc := re.pkg.FindIndex(line); loc != nil {
				if (commentLoc != nil && commentLoc[0] < loc[0]) ||
					(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] < loc[0]) ||
					(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
					// then it's commented out
					continue
				}

				if match := re.pkgName.FindStringSubmatch(string(line)); match != nil {
					if pkg, err := cxgo0.PRGRM0.GetPackage(match[len(match)-1]); err != nil {
						// it should be already present
						panic(err)
					} else {
						prePkg = pkg
					}
				}
			}

			// finally, if we read a "var" and we're in global scope, we add the global without any type
			// the type will be determined later on
			if loc := re.gl.FindIndex(line); loc != nil {
				if (commentLoc != nil && commentLoc[0] < loc[0]) ||
					(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] < loc[0]) ||
					(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) || inBlock != 0 {
					// then it's commented out or inside a block
					continue
				}

				if match := re.glName.FindStringSubmatch(string(line)); match != nil {
					if _, err := prePkg.GetGlobal(match[len(match)-1]); err != nil {
						// then it hasn't been added
						arg := cxcore.MakeArgument(match[len(match)-1], "", 0)
						arg.Offset = -1
						arg.Package = prePkg
						prePkg.AddGlobal(arg)
					}
				}
			}
		}
		stopL2x()
	}
	stopL2()

	_, stopL3 := cxprof.StartProfile(lg.l3)
	for i, source := range srcStrs {
		_, stopL3x := cxprof.StartProfile(lg.l3.WithField("src_file", source))
		source = source + "\n"
		if len(srcNames) > 0 {
			cxgo0.CurrentFileName = srcNames[i]
		}
		parseErrors += cxgo0.Parse(source)
		stopL3x()
	}
	stopL3()

	return parseErrors
}

// ParseSourceCode takes a group of files representing CX `sourceCode` and
// parses it into CX program structures for `PRGRM`.
func ParseSourceCode(sourceCode []*os.File, fileNames []string) int {
	cxgo0.PRGRM0 = actions.PRGRM

	// Copy the contents of the file pointers containing the CX source
	// code into sourceCodeCopy
	sourceCodeCopy := make([]string, len(sourceCode))
	for i, source := range sourceCode {
		tmp := bytes.NewBuffer(nil)
		io.Copy(tmp, source)
		sourceCodeCopy[i] = string(tmp.Bytes())
	}

	// We need to traverse the elements by hierarchy first add all the
	// packages and structs at the same time then add globals, as these
	// can be of a custom type (and it could be imported) the signatures
	// of functions and methods are added in the cxgo0.y pass
	parseErrors := 0
	if len(sourceCode) > 0 {
		parseErrors = Step0(sourceCodeCopy, fileNames)
	}

	actions.PRGRM.SelectProgram()

	actions.PRGRM = cxgo0.PRGRM0
	if cxcore.FoundCompileErrors || parseErrors > 0 {
		return cxcore.CX_COMPILATION_ERROR
	}

	// Adding global variables `OS_ARGS` to the `os` (operating system)
	// package.
	if osPkg, err := actions.PRGRM.GetPackage(cxcore.OS_PKG); err == nil {
		if _, err := osPkg.GetGlobal(cxcore.OS_ARGS); err != nil {
			arg0 := cxcore.MakeArgument(cxcore.OS_ARGS, "", -1).AddType(cxcore.TypeNames[cxcore.TYPE_UNDEFINED])
			arg0.Package = osPkg

			arg1 := cxcore.MakeArgument(cxcore.OS_ARGS, "", -1).AddType(cxcore.TypeNames[cxcore.TYPE_STR])
			arg1 = actions.DeclarationSpecifiers(arg1, []int{0}, cxcore.DECL_BASIC)
			arg1 = actions.DeclarationSpecifiers(arg1, []int{0}, cxcore.DECL_SLICE)

			actions.DeclareGlobalInPackage(osPkg, arg0, arg1, nil, false)
		}
	}

	_, stopL4 := cxprof.StartProfile(lg.l4)

	// The last pass of parsing that generates the actual output.
	for i, source := range sourceCodeCopy {
		// Because of an unknown reason, sometimes some CX programs
		// throw an error related to a premature EOF (particularly in Windows).
		// Adding a newline character solves this.
		source = source + "\n"
		actions.LineNo = 1
		b := bytes.NewBufferString(source)
		if len(fileNames) > 0 {
			actions.CurrentFile = fileNames[i]
		}
		_, stopL4x := cxprof.StartProfile(lg.l4.WithField("src_file", actions.CurrentFile))
		parseErrors += parser.Parse(parser.NewLexer(b))
		stopL4x()
	}
	stopL4()

	if cxcore.FoundCompileErrors || parseErrors > 0 {
		return cxcore.CX_COMPILATION_ERROR
	}

	return 0
}

func identifyPackagesAndStructs(filename, srcStr string) {
	if lg.l1 != nil {
		_, stopL1x := cxprof.StartProfile(lg.l1.WithField("filename", filename))
		defer stopL1x()
	}

	var (
		row         = 0     // row number
		isCommented = false // whether the current row is commented
	)

	s := bufio.NewScanner(strings.NewReader(srcStr))
	for s.Scan() {
		line := s.Bytes()
		row++

		cf := makeCommentLocator(line)

		if skip := cf.skipLine(&isCommented, true); skip {
			continue
		}


	}
}

type commentLocator struct {
	singleOpen []int // index of single-line comment
	multiOpen  []int // index of multi-line comment open
	multiClose []int // index of multi-line comment close
}

func makeCommentLocator(line []byte) commentLocator {
	return commentLocator{
		singleOpen: re.comment.FindIndex(line),
		multiOpen:  re.multiCommentOpen.FindIndex(line),
		multiClose: re.multiCommentClose.FindIndex(line),
	}
}

func (cf commentLocator) skipLine(isCommented *bool, skipWithoutMultiClose bool) (skip bool) {
	if *isCommented {
		// If no multi-line close is detected, this line is still commented.
		if cf.multiClose == nil {
			return true
		}
		// Multi-line comment closed.
		*isCommented = false
	}

	// Detect start of multi-line comment.
	if cf.multiOpen != nil && cf.multiClose == nil {
		*isCommented = true

		if skipWithoutMultiClose {
			return true
		}
	}

	// No skip.
	return false
}

func (cf commentLocator) isLocationCommented(loc []int) bool {
	return false // TODO?
}

// // detectComment detects whether the line is a part of a comment or not.
// func detectComment(isCommented *bool, skipWithoutClose bool, line []byte) (skip bool) {
// 	var (
// 		cI      = re.comment.FindIndex(line)           // single line comment index
// 		mOpenI  = re.multiCommentOpen.FindIndex(line)  // multi line comment open index
// 		mCloseI = re.multiCommentClose.FindIndex(line) // multi line comment close index
// 	)
//
// 	if *isCommented {
// 		// If no multi-line close is detected, this line is still commented.
// 		if mCloseI == nil {
// 			return true
// 		}
// 		// Multi-line comment closed.
// 		*isCommented = false
// 	}
//
// 	// Detect start of multi-line comment.
// 	if mOpenI != nil && mCloseI == nil {
// 		*isCommented = true
//
// 		if skipWithoutClose {
// 			return true
// 		}
// 	}
//
// 	// No skip.
// 	return false
// }