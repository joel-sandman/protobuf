// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package impl

import (
	"fmt"
	"reflect"
	"sort"
	"sync"

	"google.golang.org/protobuf/internal/encoding/messageset"
	"google.golang.org/protobuf/internal/encoding/wire"
	"google.golang.org/protobuf/internal/fieldsort"
	pref "google.golang.org/protobuf/reflect/protoreflect"
	piface "google.golang.org/protobuf/runtime/protoiface"
)

// coderMessageInfo contains per-message information used by the fast-path functions.
// This is a different type from MessageInfo to keep MessageInfo as general-purpose as
// possible.
type coderMessageInfo struct {
	methods piface.Methods

	orderedCoderFields []*coderFieldInfo
	denseCoderFields   []*coderFieldInfo
	coderFields        map[wire.Number]*coderFieldInfo
	sizecacheOffset    offset
	unknownOffset      offset
	extensionOffset    offset
	needsInitCheck     bool
	isMessageSet       bool

	extensionFieldInfosMu sync.RWMutex
	extensionFieldInfos   map[pref.ExtensionType]*extensionFieldInfo
}

type coderFieldInfo struct {
	funcs      pointerCoderFuncs // fast-path per-field functions
	num        pref.FieldNumber  // field number
	offset     offset            // struct field offset
	wiretag    uint64            // field tag (number + wire type)
	tagsize    int               // size of the varint-encoded tag
	isPointer  bool              // true if IsNil may be called on the struct field
	isRequired bool              // true if field is required
}

func (mi *MessageInfo) makeCoderMethods(t reflect.Type, si structInfo) {
	mi.sizecacheOffset = si.sizecacheOffset
	mi.unknownOffset = si.unknownOffset
	mi.extensionOffset = si.extensionOffset

	mi.coderFields = make(map[wire.Number]*coderFieldInfo)
	fields := mi.Desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)

		fs := si.fieldsByNumber[fd.Number()]
		if fd.ContainingOneof() != nil {
			fs = si.oneofsByName[fd.ContainingOneof().Name()]
		}
		ft := fs.Type
		var wiretag uint64
		if !fd.IsPacked() {
			wiretag = wire.EncodeTag(fd.Number(), wireTypes[fd.Kind()])
		} else {
			wiretag = wire.EncodeTag(fd.Number(), wire.BytesType)
		}
		var fieldOffset offset
		var funcs pointerCoderFuncs
		switch {
		case fd.ContainingOneof() != nil:
			fieldOffset = offsetOf(fs, mi.Exporter)
			funcs = makeOneofFieldCoder(fd, si)
		case fd.IsWeak():
			fieldOffset = si.weakOffset
			funcs = makeWeakMessageFieldCoder(fd)
		default:
			fieldOffset = offsetOf(fs, mi.Exporter)
			funcs = fieldCoder(fd, ft)
		}
		cf := &coderFieldInfo{
			num:     fd.Number(),
			offset:  fieldOffset,
			wiretag: wiretag,
			tagsize: wire.SizeVarint(wiretag),
			funcs:   funcs,
			isPointer: (fd.Cardinality() == pref.Repeated ||
				fd.Kind() == pref.MessageKind ||
				fd.Kind() == pref.GroupKind ||
				fd.Syntax() != pref.Proto3),
			isRequired: fd.Cardinality() == pref.Required,
		}
		mi.orderedCoderFields = append(mi.orderedCoderFields, cf)
		mi.coderFields[cf.num] = cf
	}
	if messageset.IsMessageSet(mi.Desc) {
		if !mi.extensionOffset.IsValid() {
			panic(fmt.Sprintf("%v: MessageSet with no extensions field", mi.Desc.FullName()))
		}
		if !mi.unknownOffset.IsValid() {
			panic(fmt.Sprintf("%v: MessageSet with no unknown field", mi.Desc.FullName()))
		}
		mi.isMessageSet = true
	}
	sort.Slice(mi.orderedCoderFields, func(i, j int) bool {
		return mi.orderedCoderFields[i].num < mi.orderedCoderFields[j].num
	})

	var maxDense pref.FieldNumber
	for _, cf := range mi.orderedCoderFields {
		if cf.num >= 16 && cf.num >= 2*maxDense {
			break
		}
		maxDense = cf.num
	}
	mi.denseCoderFields = make([]*coderFieldInfo, maxDense+1)
	for _, cf := range mi.orderedCoderFields {
		if int(cf.num) > len(mi.denseCoderFields) {
			break
		}
		mi.denseCoderFields[cf.num] = cf
	}

	// To preserve compatibility with historic wire output, marshal oneofs last.
	if mi.Desc.Oneofs().Len() > 0 {
		sort.Slice(mi.orderedCoderFields, func(i, j int) bool {
			fi := fields.ByNumber(mi.orderedCoderFields[i].num)
			fj := fields.ByNumber(mi.orderedCoderFields[j].num)
			return fieldsort.Less(fi, fj)
		})
	}

	mi.needsInitCheck = needsInitCheck(mi.Desc)
	if mi.methods.MarshalAppend == nil && mi.methods.Size == nil {
		mi.methods.Flags |= piface.SupportMarshalDeterministic
		mi.methods.MarshalAppend = mi.marshalAppend
		mi.methods.Size = mi.size
	}
	if mi.methods.Unmarshal == nil {
		mi.methods.Flags |= piface.SupportUnmarshalDiscardUnknown
		mi.methods.Unmarshal = mi.unmarshal
	}
	if mi.methods.IsInitialized == nil {
		mi.methods.IsInitialized = mi.isInitialized
	}
}