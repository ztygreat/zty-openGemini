/*
Copyright 2022 Huawei Cloud Computing Technologies Co., Ltd.

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

package immutable

import (
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/openGemini/openGemini/lib/fileops"
	"github.com/openGemini/openGemini/lib/record"
	"go.uber.org/zap"
)

const (
	unorderedDir      = "out-of-order"
	tsspFileSuffix    = ".tssp"
	tmpTsspFileSuffix = ".init"
	tmpSuffixNameLen  = len(tmpTsspFileSuffix)
	tsspFileSuffixLen = len(tsspFileSuffix)
	compactLogDir     = "compact_log"
	DownSampleLogDir  = "downsample_log"

	TsspDirName = "tssp"

	defaultCap = 64
)

var errFileClosed = fmt.Errorf("tssp file closed")

type TSSPFile interface {
	FileName() TSSPFileName
	LevelAndSequence() (uint16, uint64)
	FileNameMerge() uint16
	FileNameExtend() uint16
	IsOrder() bool
	RefFileReader()
	UnrefFileReader()
	Stop()
	Inuse() bool
	Read(id uint64, tr record.TimeRange, dst *record.Record) (*record.Record, error)
	Delete(ids []int64) error
	DeleteRange(ids []int64, min, max int64) error
	HasTombstones() bool
	TombstoneFiles() []TombstoneFile
	LoadIdTimes(p *IdTimePairs) error
	Remove() error
	MetaIndexItemNum() int64
	AddToEvictList(level uint16)
	RemoveFromEvictList(level uint16)
	Free(evictLock bool) int64

	// TSSPFileReader
	TSSPFileReader
}

type TSSPFiles struct {
	lock    sync.RWMutex
	ref     int64
	wg      sync.WaitGroup
	closing int64
	files   []TSSPFile
}

func NewTSSPFiles() *TSSPFiles {
	return &TSSPFiles{
		files: make([]TSSPFile, 0, 32),
		ref:   1,
	}
}

func (f *TSSPFiles) fullCompacted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if len(f.files) <= 1 {
		return true
	}

	sameLeve := true
	lv, seq := f.files[0].LevelAndSequence()
	for i := 1; i < len(f.files); i++ {
		if sameLeve {
			level, curSeq := f.files[i].LevelAndSequence()
			sameLeve = lv == level && curSeq == seq
		} else {
			break
		}
	}
	return sameLeve
}

func (f *TSSPFiles) Len() int      { return len(f.files) }
func (f *TSSPFiles) Swap(i, j int) { f.files[i], f.files[j] = f.files[j], f.files[i] }
func (f *TSSPFiles) Less(i, j int) bool {
	_, iSeq := f.files[i].LevelAndSequence()
	_, jSeq := f.files[j].LevelAndSequence()
	iExt, jExt := f.files[i].FileNameExtend(), f.files[j].FileNameExtend()
	if iSeq != jSeq {
		return iSeq < jSeq
	}
	return iExt < jExt
}

func (f *TSSPFiles) StopFiles() {
	atomic.AddInt64(&f.closing, 1)
	f.lock.RLock()
	for _, tf := range f.files {
		tf.Stop()
	}
	f.lock.RUnlock()
}

func (f *TSSPFiles) fileIndex(tbl TSSPFile) int {
	if len(f.files) == 0 {
		return -1
	}

	idx := -1
	_, seq := tbl.LevelAndSequence()
	left, right := 0, f.Len()-1
	for left < right {
		mid := (left + right) / 2
		_, n := f.files[mid].LevelAndSequence()
		if seq == n {
			idx = mid
			break
		} else if seq < n {
			right = mid
		} else {
			left = mid + 1
		}
	}

	if idx != -1 {
		for i := idx; i >= 0; i-- {
			if _, n := f.files[i].LevelAndSequence(); n != seq {
				break
			}
			if f.files[i].Path() == tbl.Path() {
				return i
			}
		}

		for i := idx + 1; i < f.Len(); i++ {
			if _, n := f.files[i].LevelAndSequence(); n != seq {
				break
			}
			if f.files[i].Path() == tbl.Path() {
				return i
			}
		}
	}

	if f.files[left].Path() == tbl.Path() {
		return left
	}

	return -1
}

func (f *TSSPFiles) Files() []TSSPFile {
	return f.files
}

func (f *TSSPFiles) deleteFile(tbl TSSPFile) {
	idx := f.fileIndex(tbl)
	if idx < 0 || idx >= f.Len() {
		panic(fmt.Sprintf("file not file, %v", tbl.Path()))
	}

	f.files = append(f.files[:idx], f.files[idx+1:]...)
}

func (f *TSSPFiles) Append(file TSSPFile) {
	f.files = append(f.files, file)
}

type tsspFile struct {
	mu sync.RWMutex
	wg sync.WaitGroup

	name TSSPFileName
	ref  int32
	flag uint32 // flag > 0 indicates that the files is need close.
	lock *string

	memEle *list.Element // lru node
	reader TSSPFileReader
}

func OpenTSSPFile(name string, lockPath *string, isOrder bool, cacheData bool) (TSSPFile, error) {
	var fileName TSSPFileName
	if err := fileName.ParseFileName(name); err != nil {
		return nil, err
	}
	fileName.SetOrder(isOrder)

	fr, err := NewTSSPFileReader(name, lockPath)
	if err != nil || fr == nil {
		return nil, err
	}

	fr.inMemBlock = emptyMemReader
	if cacheData {
		idx := calcBlockIndex(int(fr.trailer.dataSize))
		fr.inMemBlock = NewMemoryReader(blockSize[idx])
	}

	if err = fr.Open(); err != nil {
		return nil, err
	}

	return &tsspFile{
		name:   fileName,
		reader: fr,
		ref:    1,
		lock:   lockPath,
	}, nil
}

func (f *tsspFile) stopped() bool {
	return atomic.LoadUint32(&f.flag) > 0
}

func (f *tsspFile) Stop() {
	atomic.AddUint32(&f.flag, 1)
}

func (f *tsspFile) Inuse() bool {
	return atomic.LoadInt32(&f.ref) > 1
}

func (f *tsspFile) Ref() {
	if f.stopped() {
		return
	}

	atomic.AddInt32(&f.ref, 1)
	f.wg.Add(1)
}

func (f *tsspFile) Unref() {
	if atomic.AddInt32(&f.ref, -1) <= 0 {
		if f.stopped() {
			return
		}
		panic("file closed")
	}
	f.wg.Done()
}

func (f *tsspFile) RefFileReader() {
	f.reader.Ref()
}

func (f *tsspFile) UnrefFileReader() {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.stopped() {
		return
	}
	f.reader.Unref()
}

func (f *tsspFile) LevelAndSequence() (uint16, uint64) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.name.level, f.name.seq
}

func (f *tsspFile) FileNameMerge() uint16 {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.name.merge
}

func (f *tsspFile) FileNameExtend() uint16 {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.name.extent
}

func (f *tsspFile) Path() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.stopped() {
		return ""
	}
	return f.reader.Path()
}

func (f *tsspFile) CreateTime() int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.reader.CreateTime()
}

func (f *tsspFile) Name() string {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.stopped() {
		return ""
	}

	return f.reader.Name()
}

func (f *tsspFile) FileName() TSSPFileName {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.name
}

func (f *tsspFile) IsOrder() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.name.order
}

func (f *tsspFile) FreeMemory() int64 {
	f.mu.Lock()
	if f.Inuse() {
		f.mu.Unlock()
		nodeTableStoreGC.Add(true, f)
		return 0
	}

	size := f.reader.FreeMemory()
	f.mu.Unlock()
	return size
}

func (f *tsspFile) Free(evictLock bool) int64 {
	size := f.FreeMemory()
	level := f.name.level
	order := f.name.order

	if order {
		addMemSize(levelName(level), -size, -size, 0)
	} else {
		addMemSize(levelName(level), -size, 0, -size)
	}

	if evictLock {
		f.RemoveFromEvictList(level)
	} else {
		f.RemoveFromEvictListUnSafe(level)
	}

	return size
}

func (f *tsspFile) FreeFileHandle() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.stopped() {
		return nil
	}
	if err := f.reader.FreeFileHandle(); err != nil {
		return err
	}
	return nil
}

func (f *tsspFile) MetaIndex(id uint64, tr record.TimeRange) (int, *MetaIndex, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.stopped() {
		return 0, nil, errFileClosed
	}
	return f.reader.MetaIndex(id, tr)
}

func (f *tsspFile) MetaIndexAt(idx int) (*MetaIndex, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.stopped() {
		return nil, errFileClosed
	}
	return f.reader.MetaIndexAt(idx)
}

func (f *tsspFile) ChunkMeta(id uint64, offset int64, size, itemCount uint32, metaIdx int, dst *ChunkMeta, buffer *[]byte) (*ChunkMeta, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.stopped() {
		return nil, errFileClosed
	}
	return f.reader.ChunkMeta(id, offset, size, itemCount, metaIdx, dst, buffer)
}

func (f *tsspFile) Read(uint64, record.TimeRange, *record.Record) (*record.Record, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	panic("impl me")
}

func (f *tsspFile) ReadData(offset int64, size uint32, dst *[]byte) ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.stopped() {
		return nil, errFileClosed
	}

	return f.reader.ReadData(offset, size, dst)
}

func (f *tsspFile) ReadChunkMetaData(metaIdx int, m *MetaIndex, dst []ChunkMeta) ([]ChunkMeta, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.stopped() {
		return dst, errFileClosed
	}

	return f.reader.ReadChunkMetaData(metaIdx, m, dst)
}

func (f *tsspFile) ReadAt(cm *ChunkMeta, segment int, dst *record.Record, decs *ReadContext) (*record.Record, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.stopped() {
		return nil, errFileClosed
	}

	if segment < 0 || segment >= cm.segmentCount() {
		err := fmt.Errorf("segment index %d out of range %d", segment, cm.segmentCount())
		log.Error(err.Error())
		return nil, err
	}

	return f.reader.ReadAt(cm, segment, dst, decs)
}

func (f *tsspFile) ChunkMetaAt(index int) (*ChunkMeta, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.stopped() {
		return nil, errFileClosed
	}
	return f.reader.ChunkMetaAt(index)
}

func (f *tsspFile) FileStat() *Trailer {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.reader.FileStat()
}

func (f *tsspFile) InMemSize() int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.reader.InMemSize()
}

func (f *tsspFile) FileSize() int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.reader.FileSize()
}

func (f *tsspFile) Contains(id uint64) (contains bool, err error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.stopped() {
		return false, errFileClosed
	}

	return f.reader.Contains(id)
}

func (f *tsspFile) ContainsValue(id uint64, tr record.TimeRange) (contains bool, err error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.stopped() {
		return false, errFileClosed
	}
	return f.reader.ContainsValue(id, tr)
}

func (f *tsspFile) ContainsTime(tr record.TimeRange) (contains bool, err error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.stopped() {
		return false, errFileClosed
	}

	return f.reader.ContainsTime(tr)
}

func (f *tsspFile) Delete([]int64) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	panic("impl me")
}

func (f *tsspFile) DeleteRange([]int64, int64, int64) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	panic("impl me")
}

func (f *tsspFile) HasTombstones() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	panic("impl me")
}

func (f *tsspFile) TombstoneFiles() []TombstoneFile {
	f.mu.RLock()
	defer f.mu.RUnlock()
	panic("impl me")
}

func (f *tsspFile) Rename(newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped() {
		return errFileClosed
	}
	return f.reader.Rename(newName)
}

func (f *tsspFile) Remove() error {
	atomic.AddUint32(&f.flag, 1)
	if atomic.AddInt32(&f.ref, -1) == 0 {
		f.wg.Wait()

		f.mu.Lock()

		name := f.reader.Path()
		memSize := f.reader.InMemSize()
		level := f.name.level
		order := f.name.order

		log.Debug("remove file", zap.String("file", name))
		_ = f.reader.Close()
		lock := fileops.FileLockOption(*f.lock)
		err := fileops.Remove(name, lock)
		if err != nil && !os.IsNotExist(err) {
			err = errRemoveFail(name, err)
			log.Error("remove file fail", zap.Error(err))
			f.mu.Unlock()
			return err
		}
		f.mu.Unlock()

		evict := memSize > 0

		if evict {
			if order {
				addMemSize(levelName(level), -memSize, -memSize, 0)
			} else {
				addMemSize(levelName(level), -memSize, 0, -memSize)
			}
			f.RemoveFromEvictList(level)
		}

	}
	return nil
}

func (f *tsspFile) Close() error {
	f.Stop()

	f.mu.Lock()
	memSize := f.reader.InMemSize()
	level := f.name.level
	order := f.name.order
	name := f.reader.Path()
	tmp := IsTempleFile(filepath.Base(name))
	f.mu.Unlock()

	f.Unref()
	f.wg.Wait()
	_ = f.reader.Close()

	if memSize > 0 && !tmp {
		if order {
			addMemSize(levelName(level), -memSize, -memSize, 0)
		} else {
			addMemSize(levelName(level), -memSize, 0, -memSize)
		}
		f.RemoveFromEvictList(level)
	}

	return nil
}

func (f *tsspFile) Open() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return nil
}

func (f *tsspFile) LoadIdTimes(p *IdTimePairs) error {
	if f.reader == nil {
		err := fmt.Errorf("disk file not init")
		log.Error("disk file not init", zap.Uint64("seq", f.name.seq), zap.Uint16("leve", f.name.level))
		return err
	}
	fr, ok := f.reader.(*tsspFileReader)
	if !ok {
		err := fmt.Errorf("LoadIdTimes: disk file isn't *TSSPFileReader type")
		log.Error("disk file isn't *TSSPFileReader", zap.Uint64("seq", f.name.seq),
			zap.Uint16("leve", f.name.level))
		return err
	}

	if err := fr.loadIdTimes(f.IsOrder(), p); err != nil {
		return err
	}

	return nil
}

func (f *tsspFile) LoadComponents() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.reader == nil {
		err := fmt.Errorf("disk file not init")
		log.Error("disk file not init", zap.Uint64("seq", f.name.seq), zap.Uint16("leve", f.name.level))
		return err
	}

	return f.reader.LoadComponents()
}

func (f *tsspFile) LoadIntoMemory() error {
	f.mu.Lock()

	if f.reader == nil {
		f.mu.Unlock()
		err := fmt.Errorf("disk file not init")
		log.Error("disk file not init", zap.Uint64("seq", f.name.seq), zap.Uint16("leve", f.name.level))
		return err
	}

	if err := f.reader.LoadIntoMemory(); err != nil {
		f.mu.Unlock()
		return err
	}

	level := f.name.level
	size := f.reader.InMemSize()
	order := f.name.order
	f.mu.Unlock()

	if order {
		addMemSize(levelName(level), size, size, 0)
	} else {
		addMemSize(levelName(level), size, 0, size)
	}
	f.AddToEvictList(level)

	return nil
}

func (f *tsspFile) Version() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.reader.Version()
}

func (f *tsspFile) MinMaxTime() (int64, int64, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.stopped() {
		return 0, 0, errFileClosed
	}
	return f.reader.MinMaxTime()
}

func (f *tsspFile) AddToEvictList(level uint16) {
	l := levelEvictListLock(level)
	if f.memEle != nil {
		panic("memEle need to nil")
	}
	f.memEle = l.PushFront(f)
	levelEvictListUnLock(level)
}

func (f *tsspFile) RemoveFromEvictList(level uint16) {
	l := levelEvictListLock(level)
	if f.memEle != nil {
		l.Remove(f.memEle)
		f.memEle = nil
	}
	levelEvictListUnLock(level)
}

func (f *tsspFile) RemoveFromEvictListUnSafe(level uint16) {
	l := levelEvictList(level)
	if f.memEle != nil {
		l.Remove(f.memEle)
		f.memEle = nil
	}
}

func (f *tsspFile) AverageChunkRows() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.reader.AverageChunkRows()
}

func (f *tsspFile) MaxChunkRows() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.reader.MaxChunkRows()
}

func (f *tsspFile) MetaIndexItemNum() int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.reader.FileStat().MetaIndexItemNum()
}

func (f *tsspFile) BlockHeader(meta *ChunkMeta, dst []record.Field) ([]record.Field, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.reader.BlockHeader(meta, dst)
}

func (f *tsspFile) MinMaxSeriesID() (min, max uint64, err error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.reader.MinMaxSeriesID()
}

func (f *tsspFile) ReadMetaBlock(metaIdx int, id uint64, offset int64, size uint32, count uint32, dst *[]byte) ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.reader.ReadMetaBlock(metaIdx, id, offset, size, count, dst)
}

func (f *tsspFile) ReadDataBlock(offset int64, size uint32, dst *[]byte) ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.reader.ReadDataBlock(offset, size, dst)
}

var (
	_ TSSPFile = (*tsspFile)(nil)
)

var compactGroupPool = sync.Pool{New: func() interface{} { return &CompactGroup{group: make([]string, 0, 8)} }}

type CompactGroup struct {
	name    string
	shardId uint64
	toLevel uint16
	group   []string

	dropping *int64
}

func NewCompactGroup(name string, toLevle uint16, count int) *CompactGroup {
	g := compactGroupPool.Get().(*CompactGroup)
	g.name = name
	g.toLevel = toLevle
	g.group = g.group[:count]
	return g
}

func (g *CompactGroup) reset() {
	g.name = ""
	g.shardId = 0
	g.toLevel = 0
	g.group = g.group[:0]
	g.dropping = nil
}

func (g *CompactGroup) release() {
	g.reset()
	compactGroupPool.Put(g)
}

type FilesInfo struct {
	name         string // measurement name with version
	shId         uint64
	dropping     *int64
	compIts      FileIterators
	oldFiles     []TSSPFile
	oldFids      []string
	maxColumns   int
	maxChunkRows int
	avgChunkRows int
	estimateSize int
	maxChunkN    int
	toLevel      uint16
}

func GetTmpTsspFileSuffix() string {
	return tmpTsspFileSuffix
}

func FileOperation(f TSSPFile, op func()) {
	if op == nil {
		return
	}

	f.Ref()
	f.RefFileReader()
	defer func() {
		f.UnrefFileReader()
		f.Unref()
	}()
	op()
}
