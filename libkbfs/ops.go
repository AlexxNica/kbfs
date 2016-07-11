// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/keybase/go-codec/codec"
)

// list codes
const (
	opsListCode IFCERFTExtCode = iota + IFCERFTExtCodeListRangeStart
)

// OpCommon are data structures needed by all ops.  It is only
// exported for serialization purposes.
type OpCommon struct {
	RefBlocks   []IFCERFTBlockPointer `codec:"r,omitempty"`
	UnrefBlocks []IFCERFTBlockPointer `codec:"u,omitempty"`
	Updates     []IFCERFTBlockUpdate  `codec:"o,omitempty"`

	codec.UnknownFieldSetHandler

	// writerInfo is the keybase username and device that generated this
	// operation.
	// Not exported; only used during conflict resolution.
	writerInfo IFCERFTWriterInfo
	// finalPath is the final resolved path to the node that this
	// operation affects in a set of MD updates.  Not exported; only
	// used during conflict resolution.
	finalPath IFCERFTPath
}

// AddRefBlock adds this block to the list of newly-referenced blocks
// for this op.
func (oc *OpCommon) AddRefBlock(ptr IFCERFTBlockPointer) {
	oc.RefBlocks = append(oc.RefBlocks, ptr)
}

// AddUnrefBlock adds this block to the list of newly-unreferenced blocks
// for this op.
func (oc *OpCommon) AddUnrefBlock(ptr IFCERFTBlockPointer) {
	oc.UnrefBlocks = append(oc.UnrefBlocks, ptr)
}

// AddUpdate adds a mapping from an old block to the new version of
// that block, for this op.
func (oc *OpCommon) AddUpdate(oldPtr IFCERFTBlockPointer, newPtr IFCERFTBlockPointer) {
	oc.Updates = append(oc.Updates, IFCERFTBlockUpdate{oldPtr, newPtr})
}

// Refs returns a slice containing all the blocks that were initially
// referenced during this op.
func (oc *OpCommon) Refs() []IFCERFTBlockPointer {
	return oc.RefBlocks
}

// Unrefs returns a slice containing all the blocks that were
// unreferenced during this op.
func (oc *OpCommon) Unrefs() []IFCERFTBlockPointer {
	return oc.UnrefBlocks
}

func (oc *OpCommon) SetWriterInfo(info IFCERFTWriterInfo) {
	oc.writerInfo = info
}

func (oc *OpCommon) GetWriterInfo() IFCERFTWriterInfo {
	return oc.writerInfo
}

func (oc *OpCommon) SetFinalPath(p IFCERFTPath) {
	oc.finalPath = p
}

func (oc *OpCommon) GetFinalPath() IFCERFTPath {
	return oc.finalPath
}

// createOp is an op representing a file or subdirectory creation
type createOp struct {
	OpCommon
	NewName string             `codec:"n"`
	Dir     IFCERFTBlockUpdate `codec:"d"`
	Type    IFCERFTEntryType   `codec:"t"`

	// If true, this create op represents half of a rename operation.
	// This op should never be persisted.
	renamed bool

	// If true, during conflict resolution the blocks of the file will
	// be copied.
	forceCopy bool

	// If this is set, ths create op needs to be turned has been
	// turned into a symlink creation locally to avoid a cycle during
	// conflict resolution, and the following field represents the
	// text of the symlink. This op should never be persisted.
	crSymPath string
}

func newCreateOp(name string, oldDir IFCERFTBlockPointer, t IFCERFTEntryType) *createOp {
	co := &createOp{
		NewName: name,
	}
	co.Dir.Unref = oldDir
	co.Type = t
	return co
}

func (co *createOp) AddUpdate(oldPtr IFCERFTBlockPointer, newPtr IFCERFTBlockPointer) {
	if oldPtr == co.Dir.Unref {
		co.Dir.Ref = newPtr
		return
	}
	co.OpCommon.AddUpdate(oldPtr, newPtr)
}

func (co *createOp) SizeExceptUpdates() uint64 {
	return uint64(len(co.NewName))
}

func (co *createOp) AllUpdates() []IFCERFTBlockUpdate {
	updates := make([]IFCERFTBlockUpdate, len(co.Updates))
	copy(updates, co.Updates)
	return append(updates, co.Dir)
}

func (co *createOp) String() string {
	res := fmt.Sprintf("create %s (%s)", co.NewName, co.Type)
	if co.renamed {
		res += " (renamed)"
	}
	return res
}

func (co *createOp) CheckConflict(renamer IFCERFTConflictRenamer, mergedOp IFCERFTOps, isFile bool) (IFCERFTCrAction, error) {
	switch realMergedOp := mergedOp.(type) {
	case *createOp:
		// Conflicts if this creates the same name and one of them
		// isn't creating a directory.
		sameName := (realMergedOp.NewName == co.NewName)
		if sameName && (realMergedOp.Type != IFCERFTDir || co.Type != IFCERFTDir) {
			if realMergedOp.Type != IFCERFTDir &&
				(co.Type == IFCERFTDir || co.crSymPath != "") {
				// Rename the merged entry only if the unmerged one is
				// a directory (or to-be-sympath'd directory) and the
				// merged one is not.
				return &renameMergedAction{
					fromName: co.NewName,
					toName:   renamer.ConflictRename(mergedOp, co.NewName),
					symPath:  co.crSymPath,
				}, nil
			}
			// Otherwise rename the unmerged entry (guaranteed to be a file).
			return &renameUnmergedAction{
				fromName: co.NewName,
				toName:   renamer.ConflictRename(co, co.NewName),
				symPath:  co.crSymPath,
			}, nil
		}

		// If they are both directories, and one of them is a rename,
		// then we have a conflict and need to rename the renamed one.
		//
		// TODO: Implement a better merging strategy for when an
		// existing directory gets into a rename conflict with another
		// existing or new directory.
		if sameName && realMergedOp.Type == IFCERFTDir && co.Type == IFCERFTDir &&
			(realMergedOp.renamed || co.renamed) {
			// Always rename the unmerged one
			return &copyUnmergedEntryAction{
				fromName: co.NewName,
				toName:   renamer.ConflictRename(co, co.NewName),
				symPath:  co.crSymPath,
				unique:   true,
			}, nil
		}
	}
	// Doesn't conflict with any rmOps, because the default action
	// will just re-create it in the merged branch as necessary.
	return nil, nil
}

func (co *createOp) GetDefaultAction(mergedPath IFCERFTPath) IFCERFTCrAction {
	if co.forceCopy {
		return &renameUnmergedAction{
			fromName: co.NewName,
			toName:   co.NewName,
			symPath:  co.crSymPath,
		}
	}
	return &copyUnmergedEntryAction{
		fromName: co.NewName,
		toName:   co.NewName,
		symPath:  co.crSymPath,
	}
}

// rmOp is an op representing a file or subdirectory removal
type rmOp struct {
	OpCommon
	OldName string             `codec:"n"`
	Dir     IFCERFTBlockUpdate `codec:"d"`

	// Indicates that the resolution process should skip this rm op.
	// Likely indicates the rm half of a cycle-creating rename.
	dropThis bool
}

func newRmOp(name string, oldDir IFCERFTBlockPointer) *rmOp {
	ro := &rmOp{
		OldName: name,
	}
	ro.Dir.Unref = oldDir
	return ro
}

func (ro *rmOp) AddUpdate(oldPtr IFCERFTBlockPointer, newPtr IFCERFTBlockPointer) {
	if oldPtr == ro.Dir.Unref {
		ro.Dir.Ref = newPtr
		return
	}
	ro.OpCommon.AddUpdate(oldPtr, newPtr)
}

func (ro *rmOp) SizeExceptUpdates() uint64 {
	return uint64(len(ro.OldName))
}

func (ro *rmOp) AllUpdates() []IFCERFTBlockUpdate {
	updates := make([]IFCERFTBlockUpdate, len(ro.Updates))
	copy(updates, ro.Updates)
	return append(updates, ro.Dir)
}

func (ro *rmOp) String() string {
	return fmt.Sprintf("rm %s", ro.OldName)
}

func (ro *rmOp) CheckConflict(renamer IFCERFTConflictRenamer, mergedOp IFCERFTOps, isFile bool) (IFCERFTCrAction, error) {
	switch realMergedOp := mergedOp.(type) {
	case *createOp:
		if realMergedOp.NewName == ro.OldName {
			// Conflicts if this creates the same name.  This can only
			// happen if the merged branch deleted the old node and
			// re-created it, in which case it is totally fine to drop
			// this rm op for the original node.
			return &dropUnmergedAction{op: ro}, nil
		}
	case *rmOp:
		if realMergedOp.OldName == ro.OldName {
			// Both removed the same file.
			return &dropUnmergedAction{op: ro}, nil
		}
	}
	return nil, nil
}

func (ro *rmOp) GetDefaultAction(mergedPath IFCERFTPath) IFCERFTCrAction {
	if ro.dropThis {
		return &dropUnmergedAction{op: ro}
	}
	return &rmMergedEntryAction{name: ro.OldName}
}

// renameOp is an op representing a rename of a file/subdirectory from
// one directory to another.  If this is a rename within the same
// directory, NewDir will be equivalent to blockUpdate{}.  renameOp
// records the moved pointer, even though it doesn't change as part of
// the operation, to make it possible to track the full path of
// directories for the purposes of conflict resolution.
type renameOp struct {
	OpCommon
	OldName     string              `codec:"on"`
	OldDir      IFCERFTBlockUpdate  `codec:"od"`
	NewName     string              `codec:"nn"`
	NewDir      IFCERFTBlockUpdate  `codec:"nd"`
	Renamed     IFCERFTBlockPointer `codec:"re"`
	RenamedType IFCERFTEntryType    `codec:"rt"`
}

func newRenameOp(oldName string, oldOldDir IFCERFTBlockPointer, newName string, oldNewDir IFCERFTBlockPointer, renamed IFCERFTBlockPointer, renamedType IFCERFTEntryType) *renameOp {
	ro := &renameOp{
		OldName:     oldName,
		NewName:     newName,
		Renamed:     renamed,
		RenamedType: renamedType,
	}
	ro.OldDir.Unref = oldOldDir
	// If we are renaming within a directory, let the NewDir remain empty.
	if oldOldDir != oldNewDir {
		ro.NewDir.Unref = oldNewDir
	}
	return ro
}

func (ro *renameOp) AddUpdate(oldPtr IFCERFTBlockPointer, newPtr IFCERFTBlockPointer) {
	if oldPtr == ro.OldDir.Unref {
		ro.OldDir.Ref = newPtr
		return
	}
	if ro.NewDir != (IFCERFTBlockUpdate{}) && oldPtr == ro.NewDir.Unref {
		ro.NewDir.Ref = newPtr
		return
	}
	ro.OpCommon.AddUpdate(oldPtr, newPtr)
}

func (ro *renameOp) SizeExceptUpdates() uint64 {
	return uint64(len(ro.NewName) + len(ro.NewName))
}

func (ro *renameOp) AllUpdates() []IFCERFTBlockUpdate {
	updates := make([]IFCERFTBlockUpdate, len(ro.Updates))
	copy(updates, ro.Updates)
	if (ro.NewDir != IFCERFTBlockUpdate{}) {
		return append(updates, ro.NewDir, ro.OldDir)
	}
	return append(updates, ro.OldDir)
}

func (ro *renameOp) String() string {
	return fmt.Sprintf("rename %s -> %s", ro.OldName, ro.NewName)
}

func (ro *renameOp) CheckConflict(renamer IFCERFTConflictRenamer, mergedOp IFCERFTOps, isFile bool) (IFCERFTCrAction, error) {
	return nil, fmt.Errorf("Unexpected conflict check on a rename op: %s", ro)
}

func (ro *renameOp) GetDefaultAction(mergedPath IFCERFTPath) IFCERFTCrAction {
	return nil
}

// syncOp is an op that represents a series of writes to a file.
type syncOp struct {
	OpCommon
	File   IFCERFTBlockUpdate  `codec:"f"`
	Writes []IFCERFTWriteRange `codec:"w"`
}

func newSyncOp(oldFile IFCERFTBlockPointer) *syncOp {
	so := &syncOp{}
	so.File.Unref = oldFile
	so.resetUpdateState()
	return so
}

func (so *syncOp) resetUpdateState() {
	so.Updates = nil
}

func (so *syncOp) AddUpdate(oldPtr IFCERFTBlockPointer, newPtr IFCERFTBlockPointer) {
	if oldPtr == so.File.Unref {
		so.File.Ref = newPtr
		return
	}
	so.OpCommon.AddUpdate(oldPtr, newPtr)
}

func (so *syncOp) addWrite(off uint64, length uint64) IFCERFTWriteRange {
	latestWrite := IFCERFTWriteRange{Off: off, Len: length}
	so.Writes = append(so.Writes, latestWrite)
	return latestWrite
}

func (so *syncOp) addTruncate(off uint64) IFCERFTWriteRange {
	latestWrite := IFCERFTWriteRange{Off: off, Len: 0}
	so.Writes = append(so.Writes, latestWrite)
	return latestWrite
}

func (so *syncOp) SizeExceptUpdates() uint64 {
	return uint64(len(so.Writes) * 16)
}

func (so *syncOp) AllUpdates() []IFCERFTBlockUpdate {
	updates := make([]IFCERFTBlockUpdate, len(so.Updates))
	copy(updates, so.Updates)
	return append(updates, so.File)
}

func (so *syncOp) String() string {
	var writes []string
	for _, r := range so.Writes {
		writes = append(writes, fmt.Sprintf("{off=%d, len=%d}", r.Off, r.Len))
	}
	return fmt.Sprintf("sync [%s]", strings.Join(writes, ", "))
}

func (so *syncOp) CheckConflict(renamer IFCERFTConflictRenamer, mergedOp IFCERFTOps, isFile bool) (IFCERFTCrAction, error) {
	switch mergedOp.(type) {
	case *syncOp:
		// Any sync on the same file is a conflict.  (TODO: add
		// type-specific intelligent conflict resolvers for file
		// contents?)
		return &renameUnmergedAction{
			fromName: so.GetFinalPath().TailName(),
			toName: renamer.ConflictRename(so, mergedOp.GetFinalPath().
				TailName()),
			unmergedParentMostRecent: so.GetFinalPath().ParentPath().
				TailPointer(),
			mergedParentMostRecent: mergedOp.GetFinalPath().ParentPath().
				TailPointer(),
		}, nil
	case *setAttrOp:
		// Someone on the merged path explicitly set an attribute, so
		// just copy the size and blockpointer over.
		return &copyUnmergedAttrAction{
			fromName: so.GetFinalPath().TailName(),
			toName:   mergedOp.GetFinalPath().TailName(),
			attr:     []attrChange{sizeAttr},
		}, nil
	}
	return nil, nil
}

func (so *syncOp) GetDefaultAction(mergedPath IFCERFTPath) IFCERFTCrAction {
	return &copyUnmergedEntryAction{
		fromName: so.GetFinalPath().TailName(),
		toName:   mergedPath.TailName(),
		symPath:  "",
	}
}

// In the functions below. a collapsed []WriteRange is a sequence of
// non-overlapping writes with strictly increasing Off, and maybe a
// trailing truncate (with strictly greater Off).

// coalesceWrites combines the given `wNew` with the head and tail of
// the given collapsed `existingWrites` slice.  For example, if the
// new write is {5, 100}, and `existingWrites` = [{7,5}, {18,10},
// {98,10}], the returned write will be {5,103}.  There may be a
// truncate at the end of the returned slice as well.
func coalesceWrites(existingWrites []IFCERFTWriteRange, wNew IFCERFTWriteRange) []IFCERFTWriteRange {
	if wNew.IsTruncate() {
		panic("coalesceWrites cannot be called with a new truncate.")
	}
	if len(existingWrites) == 0 {
		return []IFCERFTWriteRange{wNew}
	}
	newOff := wNew.Off
	newEnd := wNew.End()
	wOldHead := existingWrites[0]
	wOldTail := existingWrites[len(existingWrites)-1]
	if !wOldTail.IsTruncate() && wOldTail.End() > newEnd {
		newEnd = wOldTail.End()
	}
	if !wOldHead.IsTruncate() && wOldHead.Off < newOff {
		newOff = wOldHead.Off
	}
	ret := []IFCERFTWriteRange{{Off: newOff, Len: newEnd - newOff}}
	if wOldTail.IsTruncate() {
		ret = append(ret, IFCERFTWriteRange{Off: newEnd})
	}
	return ret
}

// Assumes writes is already collapsed, i.e. a sequence of
// non-overlapping writes with strictly increasing Off, and maybe a
// trailing truncate (with strictly greater Off).
func addToCollapsedWriteRange(writes []IFCERFTWriteRange, wNew IFCERFTWriteRange) []IFCERFTWriteRange {
	// Form three regions: head, mid, and tail: head is the maximal prefix
	// of writes less than (with respect to Off) and unaffected by wNew,
	// tail is the maximal suffix of writes greater than (with respect to
	// Off) and unaffected by wNew, and mid is everything else, i.e. the
	// range of writes affected by wNew.
	var headEnd int
	for ; headEnd < len(writes); headEnd++ {
		wOld := writes[headEnd]
		if wOld.Off >= wNew.Off || wNew.Affects(wOld) {
			break
		}
	}
	head := writes[:headEnd]

	if wNew.IsTruncate() {
		// end is empty, since a truncate affects a suffix of writes.
		mid := writes[headEnd:]

		if len(mid) == 0 {
			// Truncate past the last write.
			return append(head, wNew)
		} else if mid[0].IsTruncate() {
			// Min truncate wins
			if mid[0].Off < wNew.Off {
				return append(head, mid[0])
			}
			return append(head, wNew)
		} else if mid[0].Off < wNew.Off {
			return append(head, IFCERFTWriteRange{
				Off: mid[0].Off,
				Len: wNew.Off - mid[0].Off,
			}, wNew)
		}
		return append(head, wNew)
	}

	// wNew is a write.

	midEnd := headEnd
	for ; midEnd < len(writes); midEnd++ {
		wOld := writes[midEnd]
		if !wNew.Affects(wOld) {
			break
		}
	}

	mid := writes[headEnd:midEnd]
	end := writes[midEnd:]
	mid = coalesceWrites(mid, wNew)
	return append(head, append(mid, end...)...)
}

// collapseWriteRange returns a set of writes that represent the final
// dirty state of this file after this syncOp, given a previous write
// range.  It coalesces overlapping dirty writes, and it erases any
// writes that occurred before a truncation with an offset smaller
// than its max dirty byte.
//
// This function assumes that `writes` has already been collapsed (or
// is nil).
//
// NOTE: Truncates past a file's end get turned into writes by
// folderBranchOps, but in the future we may have bona fide truncate
// WriteRanges past a file's end.
func (so *syncOp) collapseWriteRange(writes []IFCERFTWriteRange) (
	newWrites []IFCERFTWriteRange) {
	newWrites = writes
	for _, wNew := range so.Writes {
		newWrites = addToCollapsedWriteRange(newWrites, wNew)
	}
	return newWrites
}

type attrChange uint16

const (
	exAttr attrChange = iota
	mtimeAttr
	sizeAttr // only used during conflict resolution
)

func (ac attrChange) String() string {
	switch ac {
	case exAttr:
		return "ex"
	case mtimeAttr:
		return "mtime"
	case sizeAttr:
		return "size"
	}
	return "<invalid attrChange>"
}

// setAttrOp is an op that represents changing the attributes of a
// file/subdirectory with in a directory.
type setAttrOp struct {
	OpCommon
	Name string              `codec:"n"`
	Dir  IFCERFTBlockUpdate  `codec:"d"`
	Attr attrChange          `codec:"a"`
	File IFCERFTBlockPointer `codec:"f"`
}

func newSetAttrOp(name string, oldDir IFCERFTBlockPointer, attr attrChange, file IFCERFTBlockPointer) *setAttrOp {
	sao := &setAttrOp{
		Name: name,
	}
	sao.Dir.Unref = oldDir
	sao.Attr = attr
	sao.File = file
	return sao
}

func (sao *setAttrOp) AddUpdate(oldPtr IFCERFTBlockPointer, newPtr IFCERFTBlockPointer) {
	if oldPtr == sao.Dir.Unref {
		sao.Dir.Ref = newPtr
		return
	}
	sao.OpCommon.AddUpdate(oldPtr, newPtr)
}

func (sao *setAttrOp) SizeExceptUpdates() uint64 {
	return uint64(len(sao.Name))
}

func (sao *setAttrOp) AllUpdates() []IFCERFTBlockUpdate {
	updates := make([]IFCERFTBlockUpdate, len(sao.Updates))
	copy(updates, sao.Updates)
	return append(updates, sao.Dir)
}

func (sao *setAttrOp) String() string {
	return fmt.Sprintf("setAttr %s (%s)", sao.Name, sao.Attr)
}

func (sao *setAttrOp) CheckConflict(renamer IFCERFTConflictRenamer, mergedOp IFCERFTOps, isFile bool) (IFCERFTCrAction, error) {
	switch realMergedOp := mergedOp.(type) {
	case *setAttrOp:
		if realMergedOp.Attr == sao.Attr {
			var symPath string
			var causedByAttr attrChange
			if !isFile {
				// A directory has a conflict on an mtime attribute.
				// Create a symlink entry with the unmerged mtime
				// pointing to the merged entry.
				symPath = mergedOp.GetFinalPath().TailName()
				causedByAttr = sao.Attr
			}

			// A set attr for the same attribute on the same file is a
			// conflict.
			return &renameUnmergedAction{
				fromName: sao.GetFinalPath().TailName(),
				toName: renamer.ConflictRename(
					sao, mergedOp.GetFinalPath().TailName()),
				symPath:      symPath,
				causedByAttr: causedByAttr,
				unmergedParentMostRecent: sao.GetFinalPath().ParentPath().
					TailPointer(),
				mergedParentMostRecent: mergedOp.GetFinalPath().ParentPath().
					TailPointer(),
			}, nil
		}
	}
	return nil, nil
}

func (sao *setAttrOp) GetDefaultAction(mergedPath IFCERFTPath) IFCERFTCrAction {
	return &copyUnmergedAttrAction{
		fromName: sao.GetFinalPath().TailName(),
		toName:   mergedPath.TailName(),
		attr:     []attrChange{sao.Attr},
	}
}

// resolutionOp is an op that represents the block changes that took
// place as part of a conflict resolution.
type resolutionOp struct {
	OpCommon
}

func newResolutionOp() *resolutionOp {
	ro := &resolutionOp{}
	return ro
}

func (ro *resolutionOp) SizeExceptUpdates() uint64 {
	return 0
}

func (ro *resolutionOp) AllUpdates() []IFCERFTBlockUpdate {
	return ro.Updates
}

func (ro *resolutionOp) String() string {
	return "resolution"
}

func (ro *resolutionOp) CheckConflict(renamer IFCERFTConflictRenamer, mergedOp IFCERFTOps, isFile bool) (IFCERFTCrAction, error) {
	return nil, nil
}

func (ro *resolutionOp) GetDefaultAction(mergedPath IFCERFTPath) IFCERFTCrAction {
	return nil
}

// rekeyOp is an op that represents a rekey on a TLF.
type rekeyOp struct {
	OpCommon
}

func newRekeyOp() *rekeyOp {
	ro := &rekeyOp{}
	return ro
}

func (ro *rekeyOp) SizeExceptUpdates() uint64 {
	return 0
}

func (ro *rekeyOp) AllUpdates() []IFCERFTBlockUpdate {
	return ro.Updates
}

func (ro *rekeyOp) String() string {
	return "rekey"
}

func (ro *rekeyOp) CheckConflict(renamer IFCERFTConflictRenamer, mergedOp IFCERFTOps, isFile bool) (IFCERFTCrAction, error) {
	return nil, nil
}

func (ro *rekeyOp) GetDefaultAction(mergedPath IFCERFTPath) IFCERFTCrAction {
	return nil
}

// gcOp is an op that represents garbage-collecting the history of a
// folder (which may involve unreferencing blocks that previously held
// operation lists.  It may contain unref blocks before it is added to
// the metadata ops list.
type gcOp struct {
	OpCommon

	// LatestRev is the most recent MD revision that was
	// garbage-collected with this operation.
	//
	// The codec name overrides the one for RefBlocks in OpCommon,
	// which gcOp doesn't use.
	LatestRev IFCERFTMetadataRevision `codec:"r"`
}

func newGCOp(latestRev IFCERFTMetadataRevision) *gcOp {
	gco := &gcOp{
		LatestRev: latestRev,
	}
	return gco
}

func (gco *gcOp) SizeExceptUpdates() uint64 {
	return bpSize * uint64(len(gco.UnrefBlocks))
}

func (gco *gcOp) AllUpdates() []IFCERFTBlockUpdate {
	return gco.Updates
}

func (gco *gcOp) String() string {
	return fmt.Sprintf("gc %d", gco.LatestRev)
}

func (gco *gcOp) CheckConflict(renamer IFCERFTConflictRenamer, mergedOp IFCERFTOps, isFile bool) (IFCERFTCrAction, error) {
	return nil, nil
}

func (gco *gcOp) GetDefaultAction(mergedPath IFCERFTPath) IFCERFTCrAction {
	return nil
}

// invertOpForLocalNotifications returns an operation that represents
// an undoing of the effect of the given op.  These are intended to be
// used for local notifications only, and would not be useful for
// finding conflicts (for example, we lose information about the type
// of the file in a rmOp that we are trying to re-create).
func invertOpForLocalNotifications(oldOp IFCERFTOps) IFCERFTOps {
	var newOp IFCERFTOps
	switch op := oldOp.(type) {
	default:
		panic(fmt.Sprintf("Unrecognized operation: %v", op))
	case *createOp:
		newOp = newRmOp(op.NewName, op.Dir.Ref)
	case *rmOp:
		// Guess at the type, shouldn't be used for local notification
		// purposes.
		newOp = newCreateOp(op.OldName, op.Dir.Ref, IFCERFTFile)
	case *renameOp:
		newOp = newRenameOp(op.NewName, op.NewDir.Ref,
			op.OldName, op.OldDir.Ref, op.Renamed, op.RenamedType)
	case *syncOp:
		// Just replay the writes; for notifications purposes, they
		// will do the right job of marking the right bytes as
		// invalid.
		newOp = newSyncOp(op.File.Ref)
		newOp.(*syncOp).Writes = make([]IFCERFTWriteRange, len(op.Writes))
		copy(newOp.(*syncOp).Writes, op.Writes)
	case *setAttrOp:
		newOp = newSetAttrOp(op.Name, op.Dir.Ref, op.Attr, op.File)
	case *gcOp:
		newOp = op
	}

	// Now reverse all the block updates.  Don't bother with bare Refs
	// and Unrefs since they don't matter for local notification
	// purposes.
	for _, update := range oldOp.AllUpdates() {
		newOp.AddUpdate(update.Ref, update.Unref)
	}
	return newOp
}

// NOTE: If you're updating opPointerizer and RegisterOps, make sure
// to also update opPointerizerFuture and registerOpsFuture in
// ops_test.go.

// Our ugorji codec cannot decode our extension types as pointers, and
// we need them to be pointers so they correctly satisfy the op
// interface.  So this function simply converts them into pointers as
// needed.
func opPointerizer(iface interface{}) reflect.Value {
	switch op := iface.(type) {
	default:
		return reflect.ValueOf(iface)
	case createOp:
		return reflect.ValueOf(&op)
	case rmOp:
		return reflect.ValueOf(&op)
	case renameOp:
		return reflect.ValueOf(&op)
	case syncOp:
		return reflect.ValueOf(&op)
	case setAttrOp:
		return reflect.ValueOf(&op)
	case resolutionOp:
		return reflect.ValueOf(&op)
	case rekeyOp:
		return reflect.ValueOf(&op)
	case gcOp:
		return reflect.ValueOf(&op)
	}
}

// RegisterOps registers all op types with the given codec.
func RegisterOps(codec IFCERFTCodec) {
	codec.RegisterType(reflect.TypeOf(createOp{}), IFCERFTCreateOpCode)
	codec.RegisterType(reflect.TypeOf(rmOp{}), IFCERFTRmOpCode)
	codec.RegisterType(reflect.TypeOf(renameOp{}), IFCERFTRenameOpCode)
	codec.RegisterType(reflect.TypeOf(syncOp{}), IFCERFTSyncOpCode)
	codec.RegisterType(reflect.TypeOf(setAttrOp{}), IFCERFTSetAttrOpCode)
	codec.RegisterType(reflect.TypeOf(resolutionOp{}), IFCERFTResolutionOpCode)
	codec.RegisterType(reflect.TypeOf(rekeyOp{}), IFCERFTRekeyOpCode)
	codec.RegisterType(reflect.TypeOf(gcOp{}), IFCERFTGcOpCode)
	codec.RegisterIfaceSliceType(reflect.TypeOf(IFCERFTOpsList{}), opsListCode,
		opPointerizer)
}
