package qcow2

/*
Copyright (c) 2023 Yunpeng Deng
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:
The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.
THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

import (
	"container/list"
	"encoding/binary"
	"fmt"
	"os"
	"unsafe"
)

func newQcow2Driver() *BlockDriver {
	return &BlockDriver{
		FormatName:           "qcow2",
		IsFormat:             true,
		SupportBacking:       true,
		bdrv_close:           qcow2_close,
		bdrv_create:          qcow2_create,
		bdrv_open:            qcow2_open,
		bdrv_flush_to_os:     qcow2_flush_to_os,
		bdrv_pwritev_part:    qcow2_pwritev_part,
		bdrv_preadv_part:     qcow2_preadv_part,
		bdrv_block_status:    qcow2_block_status,
		bdrv_pwrite_zeroes:   qcow2_pwrite_zeroes,
		bdrv_copy_range_from: qcow2_copy_range_from,
		bdrv_copy_range_to:   qcow2_copy_range_to,
	}
}

func qcow2_close(bs *BlockDriverState) {
	if bs == nil {
		return
	}
	s := bs.opaque.(*BDRVQcow2State)
	qcow2_cache_flush(bs, s.L2TableCache)
	qcow2_cache_flush(bs, s.RefcountBlockCache)
	s.L1Table = nil
	qcow2_cache_destroy(s.L2TableCache)
	qcow2_cache_destroy(s.RefcountBlockCache)
}

func qcow2_create(filename string, options map[string]any) error {

	var err error
	var size uint64
	var backingFile string
	var child *BdrvChild
	var enableSc bool

	//check file name
	if filename == "" {
		return Err_IncompleteParameters
	}

	//check file size
	if val, ok := options[OPT_SIZE]; !ok {
		return Err_IncompleteParameters
	} else {
		size = interface2uint64(val)
	}

	//check backing file
	if val, ok := options[OPT_BACKING]; ok {
		backingFile = val.(string)
	}

	//check enable subcluster
	if val, ok := options[OPT_SUBCLUSTER]; ok {
		enableSc = val.(bool)
	}

	//now open the child
	if child, err = bdrv_open_child(filename, "raw", options, os.O_RDWR|os.O_CREATE); err != nil {
		return err
	} else {
		bdrv_set_perm(child, PERM_ALL)
	}

	//round up the size to align with the sector size(512)
	size = round_up(size, DEFAULT_SECTOR_SIZE)

	//calculate the l1size based on the cluster size
	size2 := round_up(size, DEFAULT_CLUSTER_SIZE)
	l1Size := round_up(size2, 1<<(DEFAULT_CLUSTER_BITS+DEFAULT_CLUSTER_BITS-3)) >> (DEFAULT_CLUSTER_BITS + DEFAULT_CLUSTER_BITS - 3)

	//initiate default header
	header := &QCowHeader{
		Magic:                 binary.BigEndian.Uint32(QCOW_MAGIC),
		Version:               QCOW2_VERSION3,
		BackingFileOffset:     uint64(0),
		BackingFileSize:       uint32(0),
		ClusterBits:           uint32(DEFAULT_CLUSTER_BITS),
		Size:                  uint64(size),
		CryptMethod:           uint32(QCOW2_CRYPT_METHOD),
		L1Size:                uint32(l1Size),
		L1TableOffset:         uint64(L1_TABLE_OFFSET),
		RefcountTableOffset:   uint64(REFCOUNT_TABLE_OFFSET),
		RefcountTableClusters: uint32(DEFAULT_REFCOUNT_TABLE_CLUSTERS),
		NbSnapshots:           uint32(0),
		SnapshotsOffset:       uint64(0),
		IncompatibleFeatures:  uint64(0),
		CompatibleFeatures:    uint64(0),
		AutoclearFeatures:     uint64(0),
		RefcountOrder:         uint32(QCOW2_REFCOUNT_ORDER), // NOTE: qemu now supported only refcount_order = 4
		HeaderLength:          uint32(unsafe.Sizeof(QCowHeader{})),
	}
	//set enable subcluster
	if enableSc {
		header.IncompatibleFeatures |= QCOW2_INCOMPAT_EXTL2
		header.L1Size = header.L1Size * 2
	}
	//set the backing file
	if backingFile != "" {
		header.BackingFileOffset = BACKING_FILE_OFFSET
		header.BackingFileSize = uint32(len(backingFile))
	}

	//initiate the BlockDriverState struct
	qcow2State := initiate_qcow2_state(header, enableSc)
	bs := &BlockDriverState{
		filename: filename,
		options:  make(map[string]any),
		opaque:   qcow2State,
		//SupportedWriteFlags: BDRV_REQ_WRITE_UNCHANGED | BDRV_REQ_FUA,
		SupportedWriteFlags: 0,
		RequestAlignment:    DEFAULT_ALIGNMENT,
		MaxTransfer:         DEFAULT_MAX_TRANSFER,
	}
	qcow2State.DataFile = child
	bdrv_link_child(bs, child, filename)

	//write the header to buffer in big-endian manner
	if _, err := Blk_Pwrite_Object(bs.current, 0, header, uint64(unsafe.Sizeof(*header))); err != nil {
		return err
	}

	//write the backing file
	if backingFile != "" {
		if _, err := Blk_Pwrite_Object(bs.current, BACKING_FILE_OFFSET,
			([]byte)(backingFile), uint64(len(backingFile))); err != nil {
			return err
		}
	}

	//temporary initiate cache for writing the meta information
	qcow2State.L2TableCache = qcow2_cache_create(bs, 1, qcow2State.ClusterSize)
	qcow2State.RefcountBlockCache = qcow2_cache_create(bs, 1, qcow2State.ClusterSize)

	// Write a refcount table with one refcount block
	qcow2State.RefcountTable = make([]uint64, 2*REFCOUNT_TABLE_SIZE)
	qcow2State.RefcountTable[0] = 2 * DEFAULT_CLUSTER_SIZE
	if _, err := Blk_Pwrite_Object(bs.current, REFCOUNT_TABLE_OFFSET,
		qcow2State.RefcountTable, uint64(2*REFCOUNT_TABLE_SIZE*SIZE_UINT64)); err != nil {
		return err
	}
	bdrv_flush(bs)

	//write l1 table
	qcow2State.L1Table = make([]uint64, header.L1Size)
	if _, err := Blk_Pwrite_Object(bs.current, L1_TABLE_OFFSET, qcow2State.L1Table,
		l1Size*SIZE_UINT64); err != nil {
		return err
	}
	//sync to disk
	bdrv_flush(bs)

	//alloc 4 clusters for the header block,
	//then mark them as occupied
	if _, err = qcow2_alloc_clusters(bs, 4*DEFAULT_CLUSTER_SIZE); err != nil {
		return err
	}

	//close the file
	qcow2_close(bs)
	return err
}

//func Open(bs *BlockDriverState, options *QDict, flag int) error {
func qcow2_open(filename string, opts map[string]any, flags int) (*BlockDriverState, error) {

	var err error
	var child *BdrvChild
	var backing *BdrvChild
	var enableSc bool
	var l2CacheSize uint64
	var l2CacehNum uint32

	//check file name
	if filename == "" {
		return nil, Err_IncompleteParameters
	}

	if val, ok := opts[OPT_L2CACHESIZE]; ok {
		l2CacheSize = val.(uint64)
	}

	//now open the child
	if child, err = bdrv_open_child(filename, "raw", opts, flags); err != nil {
		return nil, err
	} else {
		bdrv_set_perm(child, PERM_ALL)
	}

	//now read the header
	var header QCowHeader
	if _, err = Blk_Pread_Object(child, 0, &header, uint64(unsafe.Sizeof(header))); err != nil {
		return nil, fmt.Errorf("qcow2 file %s read fail, err: %v", filename, err)
	}
	//check header
	if err = check_Header(&header); err != nil {
		return nil, err
	}
	child.header = &header

	//read the backing file
	var backingFile string
	if header.BackingFileOffset > 0 && header.BackingFileSize > 0 {
		if _, err = Blk_Pread_Object(child, header.BackingFileOffset,
			&backingFile, uint64(header.BackingFileSize)); err != nil {
			return nil, fmt.Errorf("can not read backing file, err: %v", err)
		}
		if backing, err = bdrv_open_child(backingFile, "qcow2", opts, flags); err != nil {
			return nil, err
		} else {
			bdrv_set_perm(backing, PERM_READABLE)
		}
	}

	if header.IncompatibleFeatures&QCOW2_INCOMPAT_EXTL2 > 0 {
		enableSc = true
	}
	opaque := initiate_qcow2_state(&header, enableSc)
	opaque.DataFile = child
	//initiate the BlockDriverState struct
	bs := &BlockDriverState{
		filename:            filename,
		backingFile:         backingFile,
		opaque:              opaque, //initiate the BDRVQcow2State struct
		options:             make(map[string]any),
		SupportedWriteFlags: 0,
		RequestAlignment:    DEFAULT_ALIGNMENT,
		MaxTransfer:         DEFAULT_MAX_TRANSFER,
		TotalSectors:        header.Size / BDRV_SECTOR_SIZE,
		InheritsFrom:        nil,
	}
	//update child
	bdrv_link_child(bs, child, filename)
	//link backing
	if backing != nil {
		bdrv_link_backing(bs, backing, backingFile)
	}

	//load refcount table
	if err = qcow2_refcount_init(bs); err != nil {
		return nil, fmt.Errorf("could not initialize refcount table, err: %v", err)
	}
	//load l1 table
	if opaque.L1Size > 0 {
		opaque.L1Table = make([]uint64, opaque.L1Size)
		if _, err = Blk_Pread_Object(bs.current, opaque.L1TableOffset, opaque.L1Table,
			uint64(opaque.L1Size)*SIZE_UINT64); err != nil {
			return nil, fmt.Errorf("could not read L1 table")
		}
	}

	//initiate the caches
	if l2CacheSize > 0 {
		l2CacheSize = round_up(l2CacheSize, DEFAULT_CLUSTER_SIZE)
		l2CacehNum = uint32(l2CacheSize / DEFAULT_CLUSTER_SIZE)
	} else {
		l2CacehNum = opaque.L1Size
	}
	opaque.L2TableCache = qcow2_cache_create(bs, l2CacehNum, opaque.ClusterSize)
	//since the refcount block cache must be less than 50% of l2 table cache,
	//so 50% of l2 cache is good enough for refcount block cache
	refcountCacheNum := max(l2CacehNum/2, 1)
	opaque.RefcountBlockCache = qcow2_cache_create(bs, refcountCacheNum, opaque.ClusterSize)

	return bs, nil
}

func initiate_qcow2_state(header *QCowHeader, enableSC bool) *BDRVQcow2State {

	s := &BDRVQcow2State{
		ClusterBits:         header.ClusterBits,
		ClusterSize:         1 << header.ClusterBits,
		L1Size:              header.L1Size,
		RefcountBlockBits:   header.ClusterBits - (header.RefcountOrder - 3),
		RefcountBlockSize:   1 << (header.ClusterBits - (header.RefcountOrder - 3)),
		RefcountTableOffset: header.RefcountTableOffset,
		RefcountTableSize:   header.RefcountTableClusters << (header.ClusterBits - 3),
		ClusterOffsetMask:   1<<(70-header.ClusterBits) - 1, //only 54 bits
		L1TableOffset:       header.L1TableOffset,
		QcowVersion:         int(header.Version),
		ClusterAllocs:       list.New(),
		get_refcount:        get_refcount,
		set_refcount:        set_refcount,
	}
	//subcluster related
	if enableSC {
		s.IncompatibleFeatures |= QCOW2_INCOMPAT_EXTL2
		s.SubclustersPerCluster = QCOW_EXTL2_SUBCLUSTERS_PER_CLUSTER
		s.SubclusterBits = uint64(header.ClusterBits - 5)
		s.SubclusterSize = uint64(s.ClusterSize) / QCOW_EXTL2_SUBCLUSTERS_PER_CLUSTER
		s.L2Bits = header.ClusterBits - 4
		s.L2Size = 1 << s.L2Bits
		s.L2SliceSize = 1 << (header.ClusterBits - 4)
	} else {
		s.IncompatibleFeatures = 0
		s.SubclustersPerCluster = 1
		s.SubclusterSize = 1 << header.ClusterBits
		s.SubclusterBits = uint64(header.ClusterBits)
		s.L2Bits = header.ClusterBits - 3
		s.L2Size = 1 << (header.ClusterBits - 3)
		s.L2SliceSize = 1 << (header.ClusterBits - 3)
	}
	return s
}

func check_Header(header *QCowHeader) error {
	//check header magic
	if header.Magic != binary.BigEndian.Uint32(QCOW_MAGIC) {
		return fmt.Errorf("no qcow2 format")
	}
	//check header version
	if header.Version != QCOW2_VERSION2 && header.Version != QCOW2_VERSION3 {
		return fmt.Errorf("unsupport header version: %d", header.Version)
	}
	//check cluster bits
	if header.ClusterBits != DEFAULT_CLUSTER_BITS {
		return fmt.Errorf("no support for cluster size of %d, only 64k cluster size is supported", 1<<header.ClusterBits)
	}
	//check refcountorder
	if header.RefcountOrder != QCOW2_REFCOUNT_ORDER {
		return fmt.Errorf("no support for refcount order of %d, only 4 is supported", header.RefcountOrder)
	}
	//check crypt method
	if header.CryptMethod != QCOW2_CRYPT_METHOD {
		return fmt.Errorf("no support for cryption")
	}
	//check header length
	if header.HeaderLength > uint32(unsafe.Sizeof(QCowHeader{})) {
		return fmt.Errorf("no support for extended qcow2 header")
	}
	return nil
}

func qcow2_preadv_part(bs *BlockDriverState, offset uint64, bytes uint64,
	qiov *QEMUIOVector, qiovOffset uint64, flags BdrvRequestFlags) error {

	s := bs.opaque.(*BDRVQcow2State)
	var err error
	var curBytes uint32 /* number of bytes in current iteration */
	var hostOffset uint64
	var sctype QCow2SubclusterType

	for bytes != 0 {

		curBytes = uint32(bytes)
		s.Lock.Lock()
		err = qcow2_get_host_offset(bs, offset, &curBytes,
			&hostOffset, &sctype)
		s.Lock.Unlock()
		if err != nil {
			goto out
		}
		if sctype == QCOW2_SUBCLUSTER_ZERO_PLAIN ||
			sctype == QCOW2_SUBCLUSTER_ZERO_ALLOC ||
			(sctype == QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN && bs.backing == nil) ||
			(sctype == QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC && bs.backing == nil) {
			Qemu_Iovec_Memset(qiov, qiovOffset, 0, uint64(curBytes))
		} else {
			if err = qcow2_preadv_task(bs, sctype, hostOffset, offset, bytes, qiov, qiovOffset); err != nil {
				goto out
			}
		}
		bytes -= uint64(curBytes)
		offset += uint64(curBytes)
		qiovOffset += uint64(curBytes)
	}
out:
	return err
}

func qcow2_pwritev_part(bs *BlockDriverState, offset uint64, bytes uint64,
	qiov *QEMUIOVector, qiovOffset uint64, flags BdrvRequestFlags) error {

	s := bs.opaque.(*BDRVQcow2State)
	var err error

	var curBytes uint64 /* number of sectors in current iteration */
	var hostOffset uint64
	var l2meta *QCowL2Meta

	for bytes != 0 {

		l2meta = nil
		curBytes = bytes

		s.Lock.Lock()
		/*
		* retrieve the hostOffset which is the position in the qcow2 file for writing the buffer
		* l2meta contains all the meta information regarding the write ops.
		 */
		if err = qcow2_alloc_host_offset(bs, offset, &curBytes, &hostOffset, &l2meta); err != nil {
			goto out_locked
		}
		s.Lock.Unlock()

		err = qcow2_pwritev_task(bs, hostOffset, offset, curBytes, qiov, qiovOffset, l2meta)
		l2meta = nil /* l2meta is consumed by qcow2_co_pwritev_task() */
		if err != nil {
			goto fail_nometa
		}

		bytes -= uint64(curBytes)
		offset += uint64(curBytes)
		qiovOffset += uint64(curBytes)
	}
	err = nil
	s.Lock.Lock()

out_locked:
	//update the l2meta information, and flush it to l2 table as well as l2 cache if success.
	qcow2_handle_l2meta(bs, &l2meta, false)
	s.Lock.Unlock()
fail_nometa:
	return err
}

func qcow2_pwrite_zeroes(bs *BlockDriverState, offset uint64, bytes uint64, flags BdrvRequestFlags) error {

	var err error
	s := bs.opaque.(*BDRVQcow2State)

	head := offset_into_subcluster(s, offset)
	tail := round_up(offset+bytes, s.SubclusterSize) - (offset + bytes)
	if offset+bytes == bs.TotalSectors*BDRV_SECTOR_SIZE {
		tail = 0
	}
	if head > 0 || tail > 0 {
		var off uint64
		var nr uint32
		var sctype QCow2SubclusterType

		/* check whether remainder of cluster already reads as zero */
		if !(is_zero(bs, offset-head, head) &&
			is_zero(bs, offset+bytes, tail)) {
			return ERR_ENOTSUP
		}

		s.Lock.Lock()
		/* We can have new write after previous check */
		offset -= head
		bytes = s.SubclusterSize
		nr = uint32(s.SubclusterSize)
		err = qcow2_get_host_offset(bs, offset, &nr, &off, &sctype)
		if err != nil ||
			(sctype != QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN &&
				sctype != QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC &&
				sctype != QCOW2_SUBCLUSTER_ZERO_PLAIN &&
				sctype != QCOW2_SUBCLUSTER_ZERO_ALLOC) {
			s.Lock.Unlock()
			return err
		}
	} else {
		s.Lock.Lock()
	}

	/* Whatever is left can use real zero subclusters */
	err = qcow2_subcluster_zeroize(bs, offset, bytes, int(flags))
	s.Lock.Unlock()

	return err
}

func qcow2_handle_l2meta(bs *BlockDriverState, pl2meta **QCowL2Meta, linkL2 bool) error {
	var l2meta *QCowL2Meta
	l2meta = *pl2meta
	var err error

	s := bs.opaque.(*BDRVQcow2State)
	for l2meta != nil {

		if linkL2 {
			if err = qcow2_alloc_cluster_link_l2(bs, l2meta); err != nil {
				goto out
			}
		} else {
			qcow2_alloc_cluster_abort(bs, l2meta)
		}

		/* Take the request off the list of running requests */
		// QLIST_REMOVE(l2meta, next_in_flight);
		s.ClusterAllocs.Remove(l2meta.NextInFlight)

		//qemu_co_queue_restart_all(&l2meta->dependent_requests);
		l2meta = l2meta.Next
	}

out:
	*pl2meta = l2meta
	return err
}

func qcow2_preadv_task(bs *BlockDriverState, scType QCow2SubclusterType,
	hostOffset uint64, offset uint64, bytes uint64, qiov *QEMUIOVector, qiovOffset uint64) error {

	s := bs.opaque.(*BDRVQcow2State)
	switch scType {
	case QCOW2_SUBCLUSTER_ZERO_PLAIN, QCOW2_SUBCLUSTER_ZERO_ALLOC:
		/* Both zero types are handled in qcow2_co_preadv_part */
		panic("unexpected")

	case QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN, QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC:
		return bdrv_preadv_part(bs.backing, offset, bytes, qiov, qiovOffset, 0)

	case QCOW2_SUBCLUSTER_COMPRESSED:
		//do nothing

	case QCOW2_SUBCLUSTER_NORMAL:
		return bdrv_preadv_part(s.DataFile, hostOffset,
			bytes, qiov, qiovOffset, 0)

	default:
		panic("unexpected")
	}
	panic("unexpected")
}

func qcow2_pwritev_task(bs *BlockDriverState, hostOffset uint64, offset uint64,
	bytes uint64, qiov *QEMUIOVector, qiovOffset uint64, l2meta *QCowL2Meta) error {

	var err error
	s := bs.opaque.(*BDRVQcow2State)

	/* Try to efficiently initialize the physical space with zeroes */
	if err = handle_alloc_space(bs, l2meta); err != nil {
		goto out_unlocked
	}

	if !merge_cow(offset, bytes, qiov, qiovOffset, l2meta) {
		if err = bdrv_pwritev_part(s.DataFile, hostOffset,
			bytes, qiov, qiovOffset, 0); err != nil {
			goto out_unlocked
		}
	}

	s.Lock.Lock()
	err = qcow2_handle_l2meta(bs, &l2meta, true)
	goto out_locked

out_unlocked:
	s.Lock.Lock()

out_locked:
	qcow2_handle_l2meta(bs, &l2meta, false)
	s.Lock.Unlock()
	return err
}

func handle_alloc_space(bs *BlockDriverState, l2meta *QCowL2Meta) error {

	s := bs.opaque.(*BDRVQcow2State)
	var m *QCowL2Meta
	var err error
	if s.DataFile.bs.SupportedZeroFlags&BDRV_REQ_NO_FALLBACK == 0 {
		return nil
	}

	for m = l2meta; m != nil; m = m.Next {
		var ret bool
		startOffset := m.AllocOffset + m.CowStart.Offset
		nbBytes := m.CowEnd.Offset + m.CowEnd.NbBytes - m.CowStart.Offset

		if m.CowStart.NbBytes == 0 && m.CowEnd.NbBytes == 0 {
			continue
		}

		ret, err = is_zero_cow(bs, m)
		if err != nil {
			return err
		} else if !ret {
			continue
		}

		if err = bdrv_pwrite_zeroes(s.DataFile, startOffset, nbBytes, BDRV_REQ_NO_FALLBACK); err != nil {
			if err != ERR_ENOTSUP && err != ERR_EAGAIN {
				return err
			}
			continue
		}
		m.SkipCow = true
	}
	return nil
}

func merge_cow(offset uint64, bytes uint64, qiov *QEMUIOVector, qiovOffset uint64, l2meta *QCowL2Meta) bool {

	var m *QCowL2Meta

	for m = l2meta; m != nil; m = m.Next {
		/* If both COW regions are empty then there's nothing to merge */
		if m.CowStart.NbBytes == 0 && m.CowEnd.NbBytes == 0 {
			continue
		}
		/* If COW regions are handled already, skip this too */
		if m.SkipCow {
			continue
		}

		if l2meta_cow_start(m)+m.CowStart.NbBytes != offset {
			/* In this case the request starts before this region */
			continue
		}

		/* The write request should end immediately before the second
		 * COW region (see above for why it does not always happen) */
		if m.Offset+m.CowEnd.Offset != offset+bytes {
			continue
		}
		/* Make sure that adding both COW regions to the QEMUIOVector
		 * does not exceed IOV_MAX */
		if Qemu_Iovec_Subvec_Niov(qiov, qiovOffset, bytes) > IOV_MAX-2 {
			continue
		}

		m.DataQiov = qiov
		m.DataQiovOffset = qiovOffset
		return true
	}

	return false
}

func is_zero_cow(bs *BlockDriverState, m *QCowL2Meta) (bool, error) {
	var ret bool
	var err error
	if ret, err = bdrv_is_zero_fast(bs, m.Offset+m.CowStart.Offset,
		m.CowStart.NbBytes); err != nil {
		return ret, err
	}
	return bdrv_is_zero_fast(bs, m.Offset+m.CowEnd.Offset,
		m.CowEnd.NbBytes)
}

func is_zero(bs *BlockDriverState, offset uint64, bytes uint64) bool {
	var nr uint64
	var res uint64
	var err error

	/* Clamp to image length, before checking status of underlying sectors */
	if offset+bytes > bs.TotalSectors*BDRV_SECTOR_SIZE {
		bytes = bs.TotalSectors*BDRV_SECTOR_SIZE - offset
	}

	if bytes == 0 {
		return true
	}

	for {
		res, err = bdrv_block_status_above(bs, nil, offset, bytes, &nr, nil, nil)
		offset += nr
		bytes -= nr
		if err == nil && (res&BDRV_BLOCK_ZERO > 0) && nr > 0 && bytes > 0 {
		} else {
			break
		}
	}

	return err == nil && (res&BDRV_BLOCK_ZERO) > 0 && bytes == 0
}

func qcow2_flush_to_os(bs *BlockDriverState) error {
	s := bs.opaque.(*BDRVQcow2State)
	s.Lock.Lock()
	defer s.Lock.Unlock()
	return qcow2_write_caches(bs)
}

func qcow2_refcount_metadata_size(clusters uint64, clusterSize uint64, refcountOrder int,
	generousIncrease bool, refblockCount *uint64) (uint64, error) {
	return 0, nil
}

func qcow2_block_status(bs *BlockDriverState, wantZero bool, offset uint64,
	count uint64, pnum *uint64, tmap *uint64, file **BlockDriverState) (uint64, error) {
	s := bs.opaque.(*BDRVQcow2State)
	var hostOffset uint64
	var bytes uint32
	var scType QCow2SubclusterType
	var status uint64
	var err error

	s.Lock.Lock()
	bytes = uint32(count)
	err = qcow2_get_host_offset(bs, offset, &bytes, &hostOffset, &scType)
	s.Lock.Unlock()
	if err != nil {
		return 0, err
	}

	*pnum = uint64(bytes)

	if scType == QCOW2_SUBCLUSTER_NORMAL ||
		scType == QCOW2_SUBCLUSTER_ZERO_ALLOC ||
		scType == QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC {
		*tmap = hostOffset
		*file = s.DataFile.bs
		status |= BDRV_BLOCK_OFFSET_VALID
	}
	if scType == QCOW2_SUBCLUSTER_ZERO_PLAIN ||
		scType == QCOW2_SUBCLUSTER_ZERO_ALLOC {
		status |= BDRV_BLOCK_ZERO
	} else if scType != QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN &&
		scType != QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC {
		status |= BDRV_BLOCK_DATA
	}
	return status, nil
}

func qcow2_copy_range_from(bs *BlockDriverState, src *BdrvChild, offset uint64,
	dst *BdrvChild, dstOffset uint64, bytes uint64,
	readFlags BdrvRequestFlags, writeFlags BdrvRequestFlags) error {
	//do nothing
	fmt.Println("[qcow2_copy_range_from] no implementation")
	return nil
}

func qcow2_copy_range_to(bs *BlockDriverState, src *BdrvChild, offset uint64,
	dst *BdrvChild, dstOffset uint64, bytes uint64,
	readFlags BdrvRequestFlags, writeFlags BdrvRequestFlags) error {
	//do nothing
	fmt.Println("[qcow2_copy_range_to] no implementation")
	return nil
}