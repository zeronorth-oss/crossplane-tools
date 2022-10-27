/*
Copyright 2021 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package method

import (
	"fmt"
	"go/types"
	"strings"

	xptypes "github.com/crossplane/crossplane-tools/internal/types"
	"github.com/pkg/errors"

	"github.com/dave/jennifer/jen"
)

// NewResolveReferences returns a NewMethod that writes a ResolveReferences for
// given managed resource, if needed.
func NewResolveReferences(traverser *xptypes.Traverser, receiver, clientPath, referencePkgPath string) New {
	return func(f *jen.File, o types.Object) {
		namedType, ok := o.Type().(*types.Named)
		if !ok {
			return
		}
		refProcessor := NewReferenceProcessor(receiver,
			WithDefaultExtractor(jen.Qual(referencePkgPath, "ExternalName").Call()),
		)
		cfg := &xptypes.ProcessorConfig{
			Field: refProcessor,
			Named: xptypes.NamedProcessorChain{},
		}
		if err := traverser.Traverse(namedType, cfg); err != nil {
			panic(errors.Wrapf(err, "cannot traverse the type tree of %s", namedType.Obj().Name()))
		}
		refs := refProcessor.GetReferences()
		if len(refs) == 0 {
			return
		}
		hasMultiResolution := false
		hasSingleResolution := false
		resolverCalls := make(jen.Statement, len(refs))
		for i, ref := range refs {
			if ref.IsSlice {
				hasMultiResolution = true
				resolverCalls[i] = encapsulate(0, multiResolutionCall(ref, referencePkgPath), ref.GoValueFieldPath...).Line()
			} else {
				hasSingleResolution = true
				resolverCalls[i] = encapsulate(0, singleResolutionCall(ref, referencePkgPath), ref.GoValueFieldPath...).Line()
			}
		}
		var initStatements jen.Statement
		if hasSingleResolution {
			initStatements = append(initStatements, jen.Var().Id("rsp").Qual(referencePkgPath, "ResolutionResponse"))
		}
		if hasMultiResolution {
			initStatements = append(initStatements, jen.Line().Var().Id("mrsp").Qual(referencePkgPath, "MultiResolutionResponse"))
		}

		f.Commentf("ResolveReferences of this %s.", o.Name())
		f.Func().Params(jen.Id(receiver).Op("*").Id(o.Name())).Id("ResolveReferences").Params(jen.Id("ctx").Qual("context", "Context"), jen.Id("c").Qual(clientPath, "Reader")).Error().Block(
			jen.Id("r").Op(":=").Qual(referencePkgPath, "NewAPIResolver").Call(jen.Id("c"), jen.Id(receiver)),
			jen.Line(),
			&initStatements,
			jen.Var().Err().Error(),
			jen.Line(),
			&resolverCalls,
			jen.Line(),
			jen.Return(jen.Nil()),
		)
	}
}

var cleaner = strings.NewReplacer(
	"[]", "",
	"*", "",
)

type resolutionCallFn func(parentFields ...string) *jen.Statement

// encapsulate goes through the fields and encapsulates the final call with nil
// guard and/or for loops.
func encapsulate(index int, callFn resolutionCallFn, fields ...string) *jen.Statement {
	if len(fields) <= index {
		return callFn(fields...)
	}
	field := fields[index]
	fieldPath := jen.Id(cleaner.Replace(fields[0]))
	for i := 1; i <= index; i++ {
		fieldPath = fieldPath.Dot(cleaner.Replace(fields[i]))
	}
	switch {
	case strings.HasPrefix(field, "*"):
		fields[index] = cleaner.Replace(fields[index])
		return jen.If(fieldPath.Op("!=").Nil()).Block(encapsulate(index+1, callFn, fields...))
	case strings.HasPrefix(field, "[]"):
		fields[index] = cleaner.Replace(fields[index]) + fmt.Sprintf("[i%d]", index)
		return jen.For(
			jen.Id(fmt.Sprintf("i%d", index)).Op(":=").Lit(0),
			jen.Id(fmt.Sprintf("i%d", index)).Op("<").Len(fieldPath),
			jen.Id(fmt.Sprintf("i%d", index)).Op("++"),
		).Block(encapsulate(index+1, callFn, fields...))
	default:
		return encapsulate(index+1, callFn, fields...)
	}
}

func singleResolutionCall(ref Reference, referencePkgPath string) resolutionCallFn {
	return func(fields ...string) *jen.Statement {
		prefixPath := jen.Id(fields[0])
		for i := 1; i < len(fields)-1; i++ {
			prefixPath = prefixPath.Dot(fields[i])
		}
		currentValuePath := prefixPath.Clone().Dot(fields[len(fields)-1])
		referenceFieldPath := prefixPath.Clone().Dot(ref.GoRefFieldName)
		selectorFieldPath := prefixPath.Clone().Dot(ref.GoSelectorFieldName)

		var setResolvedValue *jen.Statement
		if ref.IsPointer {
			id := fmt.Sprintf("v%s", ref.SourceName)
			setResolvedValue = &jen.Statement{
				jen.Var().Id(id).Op(ref.SourceType.String()),
				jen.Line(),
				jen.If(
					jen.Err().Op("=").Qual(referencePkgPath, "ToPtrValue").Call(jen.Id("rsp").Dot("ResolvedValue"), jen.Id(id)),
					jen.Err().Op("!=").Nil(),
				).Block(
					jen.Return(jen.Qual("github.com/pkg/errors", "Wrap").Call(jen.Err(), jen.Lit(strings.Join(ref.GoValueFieldPath, ".")))),
				),
				jen.Line(),
				currentValuePath.Clone().Op("=").Id(id),
			}
			currentValuePath = jen.Qual(referencePkgPath, "FromPtrValue").Call(currentValuePath)
		} else {
			setResolvedValue = currentValuePath.Clone().Op("=").Id("rsp").Dot("ResolvedValue")
		}

		return &jen.Statement{
			jen.List(jen.Id("rsp"), jen.Err()).Op("=").Id("r").Dot("Resolve").Call(
				jen.Id("ctx"),
				jen.Qual(referencePkgPath, "ResolutionRequest").Values(jen.Dict{
					jen.Id("CurrentValue"): currentValuePath,
					jen.Id("Reference"):    referenceFieldPath,
					jen.Id("Selector"):     selectorFieldPath,
					jen.Id("To"): jen.Qual(referencePkgPath, "To").Values(jen.Dict{
						jen.Id("Managed"): ref.RemoteType,
						jen.Id("List"):    ref.RemoteListType,
					}),
					jen.Id("Extract"): ref.Extractor,
				},
				),
			),
			jen.Line(),
			jen.If(jen.Err().Op("!=").Nil()).Block(
				jen.Return(jen.Qual("github.com/pkg/errors", "Wrap").Call(jen.Err(), jen.Lit(strings.Join(ref.GoValueFieldPath, ".")))),
			),
			jen.Line(),
			setResolvedValue,
			jen.Line(),
			referenceFieldPath.Clone().Op("=").Id("rsp").Dot("ResolvedReference"),
			jen.Line(),
		}
	}
}

func multiResolutionCall(ref Reference, referencePkgPath string) resolutionCallFn {
	return func(fields ...string) *jen.Statement {
		prefixPath := jen.Id(fields[0])
		for i := 1; i < len(fields)-1; i++ {
			prefixPath = prefixPath.Dot(fields[i])
		}
		currentValuePath := prefixPath.Clone().Dot(fields[len(fields)-1])
		referenceFieldPath := prefixPath.Clone().Dot(ref.GoRefFieldName)
		selectorFieldPath := prefixPath.Clone().Dot(ref.GoSelectorFieldName)

		var setResolvedValues *jen.Statement

		if ref.IsPointer {
			id := fmt.Sprintf("v%s", ref.SourceName)
			setResolvedValues = &jen.Statement{
				jen.Id(id).Op(":=").Make(jen.Op(ref.SourceType.String()), jen.Len(jen.Id("mrsp").Dot("ResolvedValues"))),
				jen.Line(),
				jen.If(
					jen.Err().Op("=").Qual(referencePkgPath, "ToPtrValues").Call(jen.Id("mrsp").Dot("ResolvedValues"), jen.Id(id)),
					jen.Err().Op("!=").Nil(),
				).Block(
					jen.Return(jen.Qual("github.com/pkg/errors", "Wrap").Call(jen.Err(), jen.Lit(strings.Join(ref.GoValueFieldPath, ".")))),
				),
				jen.Line(),
				currentValuePath.Clone().Op("=").Id(id),
			}
			currentValuePath = jen.Qual(referencePkgPath, "FromPtrValues").Call(currentValuePath)
		} else {
			setResolvedValues = currentValuePath.Clone().Op("=").Id("mrsp").Dot("ResolvedValues")
		}

		return &jen.Statement{
			jen.List(jen.Id("mrsp"), jen.Err()).Op("=").Id("r").Dot("ResolveMultiple").Call(
				jen.Id("ctx"),
				jen.Qual(referencePkgPath, "MultiResolutionRequest").Values(jen.Dict{
					jen.Id("CurrentValues"): currentValuePath,
					jen.Id("References"):    referenceFieldPath,
					jen.Id("Selector"):      selectorFieldPath,
					jen.Id("To"): jen.Qual(referencePkgPath, "To").Values(jen.Dict{
						jen.Id("Managed"): ref.RemoteType,
						jen.Id("List"):    ref.RemoteListType,
					}),
					jen.Id("Extract"): ref.Extractor,
				},
				),
			),
			jen.Line(),
			jen.If(jen.Err().Op("!=").Nil()).Block(
				jen.Return(jen.Qual("github.com/pkg/errors", "Wrap").Call(jen.Err(), jen.Lit(strings.Join(ref.GoValueFieldPath, ".")))),
			),
			jen.Line(),
			setResolvedValues,
			jen.Line(),
			referenceFieldPath.Clone().Op("=").Id("mrsp").Dot("ResolvedReferences"),
			jen.Line(),
		}
	}
}
