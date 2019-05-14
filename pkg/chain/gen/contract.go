package main

//go:generate make

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"regexp"
	"strings"
	"text/template"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

// The extracted name + payability of methods from ABI JSON.
type methodPayableInfo struct {
	Name    string
	Payable bool
}

var (
	classNameRegexp *regexp.Regexp
	shortVarRegexp  *regexp.Regexp
)

func init() {
	var err error
	classNameRegexp, err = regexp.Compile("ImplV.*")
	if err != nil {
		panic(fmt.Sprintf(
			"Failed to compile class name regular expression: [%v].",
			"ImplV.*",
		))
	}

	shortVarRegexp, err = regexp.Compile("([A-Z])[^A-Z]*")
	if err != nil {
		panic(fmt.Sprintf(
			"Failed to compile class name regular expression: [%v].",
			"([A-Z])[^A-Z]*",
		))
	}
}

// The following structs are sent into the templates for compilation.
type contractInfo struct {
	Class           string
	AbiClass        string
	FullVar         string
	ShortVar        string
	ConstMethods    []methodInfo
	NonConstMethods []methodInfo
	Events          []eventInfo
}

type methodInfo struct {
	CapsName          string
	LowerName         string
	Payable           bool
	Params            string
	ParamDeclarations string
	Return            returnInfo
}

type returnInfo struct {
	Multi        bool
	Type         string
	Declarations string
	Vars         string
}

type eventInfo struct {
	CapsName                  string
	LowerName                 string
	IndexedFilters            string
	ParamExtractors           string
	ParamDeclarations         string
	IndexedFilterDeclarations string
}

// Main function. Expect <executable> [input.abi] [output.go] .
func main() {
	if len(os.Args) != 3 {
		panic(fmt.Sprintf(
			"Expected `%v [input.abi] [output.go]`, but got [%v].",
			os.Args[0],
			os.Args,
		))
	}

	abiPath := os.Args[1]
	outputPath := os.Args[2]

	abiFile, err := ioutil.ReadFile(abiPath)
	if err != nil {
		panic(fmt.Sprintf(
			"Failed to read ABI file at [%v]: [%v].",
			abiPath,
			err,
		))
	}

	outputFile, err := os.Create(outputPath)
	if err != nil {
		panic(fmt.Sprintf(
			"Failed to create Go file at [%v]: [%v].",
			outputPath,
			err,
		))
	}

	templates, err := template.ParseGlob("*.go.tmpl")
	if err != nil {
		panic(fmt.Sprintf("Failed to parse templates: [%v].", err))
	}

	abi, err := abi.JSON(strings.NewReader(string(abiFile)))
	if err != nil {
		panic(fmt.Sprintf(
			"Failed to parse ABI at [%v]: [%v].",
			abiPath,
			err,
		))
	}

	var payableInfo []methodPayableInfo
	err = json.Unmarshal(abiFile, &payableInfo)
	if err != nil {
		panic(fmt.Sprintf(
			"Failed to parse additional ABI metadata at [%v]: [%v].",
			abiPath,
			err,
		))
	}

	// The name of the ABI binding Go class is the same as the filename of the
	// ABI file, minus the extension.
	abiClassName := path.Base(abiPath)
	abiClassName = abiClassName[0 : len(abiClassName)-4] // strip .abi
	contractInfo := buildContractInfo(abiClassName, &abi, payableInfo)

	err = templates.ExecuteTemplate(outputFile, "contract.go.tmpl", contractInfo)
	if err != nil {
		panic(fmt.Sprintf(
			"Failed to generate Go file at [%v]: [%v].",
			outputPath,
			err,
		))
	}
}

func buildContractInfo(
	abiClassName string,
	abi *abi.ABI,
	payableInfo []methodPayableInfo,
) contractInfo {
	payableMethods := make(map[string]struct{})
	for _, methodPayableInfo := range payableInfo {
		if methodPayableInfo.Payable {
			payableMethods[methodPayableInfo.Name] = struct{}{}
		}
	}

	goClassName := classNameRegexp.ReplaceAll([]byte(abiClassName), nil)
	shortVar := strings.ToLower(string(shortVarRegexp.ReplaceAll(
		[]byte(goClassName),
		[]byte("$1"),
	)))
	constMethods, nonConstMethods := buildMethodInfo(payableMethods, abi.Methods)
	events := buildEventInfo(abi.Events)

	return contractInfo{
		string(goClassName),
		abiClassName,
		lowercaseFirst(string(goClassName)),
		string(shortVar),
		constMethods,
		nonConstMethods,
		events,
	}
}

func buildMethodInfo(
	payableMethods map[string]struct{},
	methodsByName map[string]abi.Method,
) (constMethods []methodInfo, nonConstMethods []methodInfo) {
	nonConstMethods = make([]methodInfo, 0, len(methodsByName))
	constMethods = make([]methodInfo, 0, len(methodsByName))

	for name, method := range methodsByName {
		_, payable := payableMethods[name]
		paramDeclarations := ""
		params := ""

		for index, param := range method.Inputs {
			goType := param.Type.Type.String()
			paramName := param.Name
			if paramName == "" {
				paramName = fmt.Sprintf("arg%v", index)
			}

			paramDeclarations += fmt.Sprintf("%v %v,\n", paramName, goType)
			params += fmt.Sprintf("%v,\n", paramName)
		}

		returned := returnInfo{}
		if len(method.Outputs) > 1 {
			returned.Multi = true
			returned.Type = strings.Replace(name, "get", "", 1)

			for _, output := range method.Outputs {
				goType := output.Type.Type.String()

				returned.Declarations += fmt.Sprintf(
					"\t%v %v\n",
					uppercaseFirst(output.Name),
					goType,
				)
				returned.Vars += fmt.Sprintf("%v,", output.Name)
			}
		} else if len(method.Outputs) == 0 {
			returned.Multi = false
		} else {
			returned.Multi = false
			returned.Type = method.Outputs[0].Type.Type.String()
			returned.Vars += "ret,"
		}

		info := methodInfo{
			uppercaseFirst(name),
			lowercaseFirst(name),
			payable,
			params,
			paramDeclarations,
			returned,
		}

		if method.Const {
			constMethods = append(constMethods, info)
		} else {
			nonConstMethods = append(nonConstMethods, info)
		}
	}

	return constMethods, nonConstMethods
}

func buildEventInfo(eventsByName map[string]abi.Event) []eventInfo {
	eventInfos := make([]eventInfo, 0, len(eventsByName))
	for name, event := range eventsByName {
		paramDeclarations := ""
		paramExtractors := ""
		indexedFilterDeclarations := ""
		indexedFilters := ""
		for _, param := range event.Inputs {
			upperParam := uppercaseFirst(param.Name)
			goType := param.Type.Type.String()

			paramDeclarations += fmt.Sprintf("%v %v,\n", upperParam, goType)
			paramExtractors += fmt.Sprintf("event.%v,\n", upperParam)
			if param.Indexed {
				indexedFilterDeclarations += fmt.Sprintf("%vFilter []%v,\n", param.Name, goType)
				indexedFilters += fmt.Sprintf("%vFilter,\n", param.Name)
			}
		}

		eventInfos = append(eventInfos, eventInfo{
			uppercaseFirst(name),
			lowercaseFirst(name),
			indexedFilters,
			paramExtractors,
			paramDeclarations,
			indexedFilterDeclarations,
		})
	}

	return eventInfos
}

func uppercaseFirst(str string) string {
	return strings.ToUpper(str[0:1]) + str[1:]
}

func lowercaseFirst(str string) string {
	return strings.ToLower(str[0:1]) + str[1:]
}
