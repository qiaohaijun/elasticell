// Copyright 2016 DeepFabric, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package raftstore

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"time"

	"github.com/deepfabric/elasticell/pkg/log"
	"github.com/deepfabric/elasticell/pkg/pb/metapb"
	"github.com/deepfabric/elasticell/pkg/pb/pdpb"
	"github.com/deepfabric/elasticell/pkg/pb/raftcmdpb"
	"github.com/deepfabric/elasticell/pkg/storage"
	"github.com/deepfabric/elasticell/pkg/util"
	"github.com/deepfabric/indexer"
	"github.com/deepfabric/indexer/cql"
	"github.com/pilosa/pilosa"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

func (s *Store) getCell(cellID uint64) (cell *metapb.Cell) {
	pr := s.replicatesMap.get(cellID)
	if pr == nil {
		return
	}
	c := pr.getStore().getCell()
	cell = &c
	return
}

func (s *Store) loadIndices() (err error) {
	if err = os.MkdirAll(filepath.Join(globalCfg.DataPath, "index"), 0700); err != nil {
		err = errors.Wrap(err, "")
		return
	}
	reExps := make(map[string]*regexp.Regexp)
	s.reExps = reExps
	docProts := make(map[string]*cql.Document)
	s.docProts = docProts
	indicesFp := filepath.Join(globalCfg.DataPath, "index", "indices.json")
	if err = util.FileUnmarshal(indicesFp, &s.indices); err != nil {
		return
	}
	for _, idxDef := range s.indices {
		reExps[idxDef.GetName()] = regexp.MustCompile(idxDef.GetKeyPattern())
	}
	var docProt *cql.DocumentWithIdx
	for _, idxDef := range s.indices {
		if docProt, err = convertToDocProt(idxDef); err != nil {
			return
		}
		docProts[idxDef.GetName()] = &docProt.Doc
	}
	return
}

func (s *Store) persistIndices() (err error) {
	indicesFp := filepath.Join(globalCfg.DataPath, "index", "indices.json")
	if err = util.FileMarshal(indicesFp, s.indices); err != nil {
		log.Errorf("store-index: failed to persist indices definition\n%+v", err)
	}
	return
}

func (s *Store) allocateDocID(cellID uint64) (docID uint64, err error) {
	var nextDocID int64
	if nextDocID, err = s.getKVEngine().IncrBy(getCellNextDocIDKey(cellID), 1); err != nil {
		return
	}
	nextDocID--
	if nextDocID&(pilosa.SliceWidth-1) == 0 {
		//If the key does not exist, it is set to 0 before performing the operation.
		if nextDocID, err = s.getKVEngine().IncrBy(getCellNextDocIDKey(0), 1); err != nil {
			return
		}
		nextDocID--
		nextDocID *= pilosa.SliceWidth
		if err = s.getKVEngine().Set(getCellNextDocIDKey(cellID), []byte(strconv.FormatInt(nextDocID+1, 10))); err != nil {
			return
		}
	}
	docID = uint64(nextDocID)
	return
}

func (s *Store) getIndexer(cellID uint64) (idxer *indexer.Indexer, err error) {
	var ok bool
	var docProt *cql.DocumentWithIdx
	s.rwlock.RLock()
	if idxer, ok = s.indexers[cellID]; ok {
		s.rwlock.RUnlock()
		return
	}
	s.rwlock.RUnlock()
	s.rwlock.Lock()
	defer s.rwlock.Unlock()
	if idxer, ok = s.indexers[cellID]; ok {
		return
	}
	indicesDir := filepath.Join(globalCfg.DataPath, "index", fmt.Sprintf("%d", cellID))
	if idxer, err = indexer.NewIndexer(indicesDir, false, globalCfg.EnableSyncRaftLog); err != nil {
		return
	}
	for _, idxDef := range s.indices {
		//creation shall be idempotent
		if docProt, err = convertToDocProt(idxDef); err != nil {
			return
		}
		if err = idxer.CreateIndex(docProt); err != nil {
			return
		}
	}
	s.indexers[cellID] = idxer
	return
}

func (s *Store) handleIndicesChange(rspIndices []*pdpb.IndexDef) (err error) {
	indicesNew := make(map[string]*pdpb.IndexDef)
	for _, idxDefNew := range rspIndices {
		indicesNew[idxDefNew.GetName()] = idxDefNew
	}
	s.rwlock.RLock()
	if reflect.DeepEqual(s.indices, indicesNew) {
		s.rwlock.RUnlock()
		return
	}
	s.rwlock.RUnlock()
	s.rwlock.Lock()
	defer s.rwlock.Unlock()
	reExpsNew := make(map[string]*regexp.Regexp)
	for _, idxDefNew := range rspIndices {
		reExpsNew[idxDefNew.GetName()] = regexp.MustCompile(idxDefNew.GetKeyPattern())
	}
	s.reExps = reExpsNew
	delta := diffIndices(s.indices, indicesNew)
	for _, idxDef := range delta.toDelete {
		//deletion shall be idempotent
		for _, idxer := range s.indexers {
			if err = idxer.DestroyIndex(idxDef.GetName()); err != nil {
				return
			}
		}
		delete(s.docProts, idxDef.GetName())
		log.Infof("store-index: deleted index %+v", idxDef)
	}
	var docProt *cql.DocumentWithIdx
	for _, idxDef := range delta.toCreate {
		//creation shall be idempotent
		if docProt, err = convertToDocProt(idxDef); err != nil {
			return
		}
		for _, idxer := range s.indexers {
			if err = idxer.CreateIndex(docProt); err != nil {
				return
			}
		}
		s.docProts[idxDef.GetName()] = &docProt.Doc
		log.Infof("store-index: created index %+v", idxDef)
	}
	if len(delta.toDelete) != 0 || len(delta.toCreate) != 0 {
		s.indices = indicesNew
		if err = s.persistIndices(); err != nil {
			return
		}
		log.Infof("store-index: persisted index definion %+v", indicesNew)
	}
	return
}

func (s *Store) readyToServeIndex(ctx context.Context) {
	tickChan := time.Tick(1 * time.Second)
	var absorbed bool
	var err error
	for {
		select {
		case <-ctx.Done():
			log.Infof("store-index: readyToServeIndex stopped")
			return
		case <-tickChan:
			for {
				// keep processing the queue until any event listed below occur:
				// - an error
				// - the queue becomes empty
				// - ctx is done
				if absorbed, err = s.handleIdxReqQueue(); err != nil {
					log.Errorf("store-index: handleIdxReqQueue failed with error\n%+v", err)
					break
				} else if absorbed {
					break
				}
				select {
				case <-ctx.Done():
					log.Infof("store-index: readyToServeIndex stopped")
					return
				default:
				}
			}
		}
	}
}

// handleIdxReqQueue handles idxReqs inside given persistent queue.
func (s *Store) handleIdxReqQueue() (absorbed bool, err error) {
	listEng := s.getListEngine()
	idxReqQueueKey := getIdxReqQueueKey()
	var idxSplitReq *pdpb.IndexSplitRequest
	var idxDestroyReq *pdpb.IndexDestroyCellRequest
	var idxRebuildReq *pdpb.IndexRebuildCellRequest
	var idxReqB []byte
	begin := time.Now()
	if idxReqB, err = listEng.LIndex(idxReqQueueKey, 0); err != nil {
		return
	}
	if idxReqB == nil || len(idxReqB) == 0 {
		absorbed = true
		return
	}
	wb := s.engine.GetKVEngine().NewWriteBatch()
	dirtyIndices := make(map[*indexer.Indexer]int)
	idxReq := &pdpb.IndexRequest{}
	if err = idxReq.Unmarshal(idxReqB); err != nil {
		return
	}
	if idxDestroyReq = idxReq.GetIdxDestroy(); idxDestroyReq != nil {
		if err = s.indexDestroyCell(idxDestroyReq, wb); err != nil {
			log.Errorf("store-index: failed to handle idxDestroyReq %+v\n%+v", idxDestroyReq, err)
		}
	} else if idxRebuildReq = idxReq.GetIdxRebuild(); idxRebuildReq != nil {
		if err = s.indexRebuildCell(idxRebuildReq, wb, dirtyIndices); err != nil {
			log.Errorf("store-index: failed to handle idxRebuildReq %+v\n%+v", idxRebuildReq, err)
		}
	} else if idxSplitReq = idxReq.GetIdxSplit(); idxSplitReq != nil {
		left := idxSplitReq.LeftCellID
		right := idxSplitReq.RightCellID
		if err = s.indexSplitCell(left, right, wb, dirtyIndices); err != nil {
			log.Errorf("store-index: failed to handle idxSplitReq %+v\n%+v", idxSplitReq, err)
		}
	} else {
		log.Errorf("store-index: unknown idxReq %v, idxReqB %v", idxReq, idxReqB)
		return
	}

	if err = s.engine.GetKVEngine().Write(wb); err != nil {
		err = errors.Wrap(err, "")
		return
	}
	for ind := range dirtyIndices {
		if err = ind.Sync(); err != nil {
			return
		}
	}
	if _, err = listEng.LPop(idxReqQueueKey); err != nil {
		err = errors.Wrap(err, "")
		return
	}
	duration := time.Now().Sub(begin)
	log.Infof("store-index: done processing idxReq %v in %v", idxReq, duration)
	return
}

func (s *Store) handleIdxKeyReq(idxKeyReq *pdpb.IndexKeyRequest) (err error) {
	key := idxKeyReq.CmdArgs[0]
	var changed bool
	var pairs []string
	wb := s.engine.GetKVEngine().NewWriteBatch()
	changed, err = s.deleteIndexedKey(key, idxKeyReq.IsDel, wb)
	if idxKeyReq.IsDel {
		if err != nil {
			log.Errorf("store-index[cell-%d]: failed to delete indexed key %+v from index %s\n%+v",
				idxKeyReq.CellID, key, idxKeyReq.GetIdxName(), err)
			return
		}
		log.Debugf("store-index[cell-%d]: deleted key %+v from index %s",
			idxKeyReq.CellID, key, idxKeyReq.GetIdxName())
	} else {
		if !changed {
			// It's an insert instead of update. Needn't invoke HGetAll.
			pairs = make([]string, len(idxKeyReq.CmdArgs)-1)
			for i := 1; i < len(idxKeyReq.CmdArgs); i++ {
				pairs[i-1] = string(idxKeyReq.CmdArgs[i])
			}
		}
		if err = s.addIndexedKey(idxKeyReq.CellID, idxKeyReq.GetIdxName(), 0, key, pairs, wb); err != nil {
			log.Errorf("store-index[cell-%d]: failed to add key %s to index %s\n%+v", idxKeyReq.CellID, key, idxKeyReq.GetIdxName(), err)
			return
		}
	}

	if err = s.engine.GetKVEngine().Write(wb); err != nil {
		err = errors.Wrap(err, "")
		return
	}
	return
}

func (s *Store) deleteIndexedKey(dataKey []byte, isDel bool, wb storage.WriteBatch) (changed bool, err error) {
	var idxer *indexer.Indexer
	var metaValB []byte
	if metaValB, err = s.engine.GetDataEngine().GetIndexInfo(dataKey); err != nil || len(metaValB) == 0 {
		return
	}
	metaVal := &pdpb.KeyMetaVal{}
	if err = metaVal.Unmarshal(metaValB); err != nil {
		err = errors.Wrap(err, "")
		return
	}
	idxName, docID, cellID := metaVal.GetIdxName(), metaVal.GetDocID(), metaVal.GetCellID()

	if idxer, err = s.getIndexer(cellID); err != nil {
		return
	}
	if _, err = idxer.Del(idxName, docID); err != nil {
		return
	}
	changed = true
	if err = wb.Delete(getDocIDKey(docID)); err != nil {
		return
	}
	if isDel {
		if err = s.engine.GetDataEngine().SetIndexInfo(dataKey, []byte{}); err != nil {
			err = errors.Wrap(err, "")
			return
		}
	}
	// If !isDel, the indexInfo will be changed later. So it's safe to skip clearing indexInfo.
	return
}

func (s *Store) addIndexedKey(cellID uint64, idxNameIn string, docID uint64, dataKey []byte, pairs []string, wb storage.WriteBatch) (err error) {
	var idxer *indexer.Indexer
	var metaVal *pdpb.KeyMetaVal
	var metaValB []byte
	var doc *cql.DocumentWithIdx
	var ok bool

	if docID == 0 {
		// allocate docID
		if docID, err = s.allocateDocID(cellID); err != nil {
			return
		}
		if err = wb.Set(getDocIDKey(docID), dataKey); err != nil {
			return
		}
	}
	metaVal = &pdpb.KeyMetaVal{
		IdxName: idxNameIn,
		DocID:   docID,
		CellID:  cellID,
	}
	if metaValB, err = metaVal.Marshal(); err != nil {
		return
	}
	if err = s.engine.GetDataEngine().SetIndexInfo(dataKey, metaValB); err != nil {
		return
	}

	var idxDef *pdpb.IndexDef
	if len(pairs) == 0 {
		var fvPairs []*raftcmdpb.FVPair
		hashEng := s.engine.GetHashEngine()
		if fvPairs, err = hashEng.HGetAll(dataKey); err != nil {
			return
		}
		pairs = make([]string, 2*len(fvPairs))
		for i, fvPair := range fvPairs {
			pairs[2*i] = string(fvPair.GetField())
			pairs[2*i+1] = string(fvPair.GetValue())
		}
	}
	if idxDef, ok = s.indices[idxNameIn]; !ok {
		err = errors.Errorf("index %s doesn't exist", idxNameIn)
		return
	}
	if idxer, err = s.getIndexer(cellID); err != nil {
		return
	}

	if doc, err = convertToDocument(idxDef, docID, pairs); err != nil {
		return
	}
	if err = idxer.Insert(doc); err != nil {
		return
	}
	log.Debugf("store-index[cell-%d]: added dataKey %+v to index %s, docID %d, paris %+v",
		cellID, dataKey, idxNameIn, docID, pairs)
	return
}

func (s *Store) indexSplitCell(cellIDL, cellIDR uint64, wb storage.WriteBatch, dirtyIndices map[*indexer.Indexer]int) (err error) {
	var cellL, cellR *metapb.Cell
	if cellL = s.getCell(cellIDL); cellL == nil {
		log.Infof("store-index[cell-%d]: ignored %+v due to left cell %d is gone.", cellIDL)
		return
	}
	if cellR = s.getCell(cellIDR); cellR == nil {
		log.Infof("store-index[cell-%d]: ignored %+v due to right cell %d is gone.", cellIDR)
		return
	}
	//cellR could has been splitted after idxSplitReq creation.
	//Use the up-to-date range to keep scan range as small as possible.
	start := encStartKey(cellR)
	end := encEndKey(cellR)

	var idxerL *indexer.Indexer
	if idxerL, err = s.getIndexer(cellIDL); err != nil {
		return
	}
	if _, err = s.getIndexer(cellIDR); err != nil {
		return
	}
	dirtyIndices[idxerL] = 0

	var scanned, indexed, cntErr int
	cntErr, err = s.engine.GetDataEngine().ScanIndexInfo(start, end, true, func(dataKey, metaValB []byte) (err error) {
		scanned++
		if metaValB == nil || len(metaValB) == 0 {
			return
		}
		metaVal := &pdpb.KeyMetaVal{}
		if err = metaVal.Unmarshal(metaValB); err != nil {
			return
		}
		idxName, docID, cellID := metaVal.GetIdxName(), metaVal.GetDocID(), metaVal.GetCellID()
		if cellID == cellIDR {
			// handleIdxKeyReq has already put key into the target cell
			return
		}
		if idxName != "" {
			var idxIsLive bool
			s.rwlock.RLock()
			_, idxIsLive = s.indices[idxName]
			s.rwlock.RUnlock()
			if idxIsLive {
				if _, err = idxerL.Del(idxName, docID); err != nil {
					return
				}
				if err = s.addIndexedKey(cellIDR, idxName, docID, dataKey, []string{}, wb); err != nil {
					return
				}
			}
			indexed++
		}
		return
	})
	log.Infof("store-index[cell-%d]: done cell split for right cell %+v, has scanned %d dataKeys, has indexed %d dataKeys, %d errors.", cellIDL, cellR, scanned, indexed, cntErr)
	return
}

func (s *Store) indexDestroyCell(idxDestroyReq *pdpb.IndexDestroyCellRequest, wb storage.WriteBatch) (err error) {
	var cell *metapb.Cell
	if cell = s.getCell(idxDestroyReq.CellID); cell == nil {
		log.Infof("store-index[cell-%d]: ignored %+v due to cell %d is gone.", idxDestroyReq.CellID, idxDestroyReq.CellID)
		return
	}
	start := encStartKey(cell)
	end := encEndKey(cell)

	var idxer *indexer.Indexer
	cellID := idxDestroyReq.GetCellID()
	if idxer, err = s.getIndexer(cellID); err != nil {
		return
	}
	s.rwlock.Lock()
	delete(s.indexers, idxDestroyReq.GetCellID())
	s.rwlock.Unlock()
	if err = idxer.Destroy(); err != nil {
		return
	}
	if err = wb.Delete(getCellNextDocIDKey(cellID)); err != nil {
		return
	}
	var scanned, indexed, cntErr int
	cntErr, err = s.engine.GetDataEngine().ScanIndexInfo(start, end, true, func(dataKey, metaValB []byte) (err error) {
		scanned++
		if metaValB == nil || len(metaValB) == 0 {
			return
		}
		metaVal := &pdpb.KeyMetaVal{}
		if err = metaVal.Unmarshal(metaValB); err != nil {
			return
		}
		docID := metaVal.GetDocID()
		if err = wb.Delete(getDocIDKey(docID)); err != nil {
			return
		}
		// Let garbage IndexInfo be there since it's harmless.
		indexed++
		return
	})
	log.Infof("store-index[cell-%d]: done cell destroy %+v, has scanned %d dataKeys, has indexed %d dataKeys, %d errors.", idxDestroyReq.CellID, idxDestroyReq, scanned, indexed, cntErr)
	return
}

func (s *Store) indexRebuildCell(idxRebuildReq *pdpb.IndexRebuildCellRequest, wb storage.WriteBatch, dirtyIndices map[*indexer.Indexer]int) (err error) {
	var cell *metapb.Cell
	if cell = s.getCell(idxRebuildReq.CellID); cell == nil {
		log.Infof("store-index[cell-%d]: ignored %+v due to cell %d is gone.", idxRebuildReq.CellID, idxRebuildReq.CellID)
		return
	}
	start := encStartKey(cell)
	end := encEndKey(cell)

	var idxer *indexer.Indexer

	cellID := idxRebuildReq.GetCellID()
	if idxer, err = s.getIndexer(cellID); err != nil {
		return
	}
	if err = idxer.Destroy(); err != nil {
		return
	}
	if err = idxer.Open(); err != nil {
		return
	}

	var scanned, indexed, cntErr int
	cntErr, err = s.engine.GetDataEngine().ScanIndexInfo(start, end, false, func(dataKey, metaValB []byte) (err error) {
		scanned++
		if metaValB != nil || len(metaValB) != 0 {
			metaVal := &pdpb.KeyMetaVal{}
			if err = metaVal.Unmarshal(metaValB); err != nil {
				return
			}
			docID := metaVal.GetDocID()
			if err = wb.Delete(getDocIDKey(docID)); err != nil {
				return
			}
		}
		idxName := s.matchIndex(getOriginKey(dataKey))
		if idxName != "" {
			if err = s.addIndexedKey(cellID, idxName, 0, dataKey, []string{}, wb); err != nil {
				return
			}
			indexed++
		}
		return
	})
	log.Infof("store-index[cell-%d]: done cell index rebuild %+v, has scanned %d dataKeys, has indexed %d dataKeys, %d errors", idxRebuildReq.CellID, idxRebuildReq, scanned, indexed, cntErr)
	return
}

func (s *Store) matchIndex(key []byte) (idxName string) {
	s.rwlock.RLock()
	for name, reExp := range s.reExps {
		matched := reExp.Match(key)
		if matched {
			idxName = name
			break
		}
	}
	s.rwlock.RUnlock()
	return
}

func convertToDocProt(idxDef *pdpb.IndexDef) (docProt *cql.DocumentWithIdx, err error) {
	uintProps := make([]*cql.UintProp, 0)
	strProps := make([]*cql.StrProp, 0)
	for _, f := range idxDef.Fields {
		switch f.GetType() {
		case pdpb.Text:
			strProps = append(strProps, &cql.StrProp{Name: f.GetName()})
		case pdpb.Uint8:
			uintProps = append(uintProps, &cql.UintProp{Name: f.GetName(), ValLen: 1})
		case pdpb.Uint16:
			uintProps = append(uintProps, &cql.UintProp{Name: f.GetName(), ValLen: 2})
		case pdpb.Uint32:
			uintProps = append(uintProps, &cql.UintProp{Name: f.GetName(), ValLen: 4})
		case pdpb.Uint64:
			uintProps = append(uintProps, &cql.UintProp{Name: f.GetName(), ValLen: 8})
		case pdpb.Float32:
			uintProps = append(uintProps, &cql.UintProp{Name: f.GetName(), ValLen: 4, IsFloat: true})
		case pdpb.Float64:
			uintProps = append(uintProps, &cql.UintProp{Name: f.GetName(), ValLen: 8, IsFloat: true})
		default:
			err = errors.Errorf("invalid field type %+v of idxDef %+v", f.GetType().String(), idxDef)
			return
		}
	}
	docProt = &cql.DocumentWithIdx{
		Doc: cql.Document{
			DocID:     0,
			UintProps: uintProps,
			StrProps:  strProps,
		},
		Index: idxDef.GetName(),
	}
	return
}

func convertToDocument(idxDef *pdpb.IndexDef, docID uint64, pairs []string) (doc *cql.DocumentWithIdx, err error) {
	doc = &cql.DocumentWithIdx{
		Doc: cql.Document{
			DocID:     docID,
			UintProps: make([]*cql.UintProp, 0),
			StrProps:  make([]*cql.StrProp, 0),
		},
		Index: idxDef.GetName(),
	}
	log.Debugf("store-index: idxDef %+v, docID %+v, pairs %+v", idxDef, docID, pairs)
	for i := 0; i < len(pairs); i += 2 {
		field := pairs[i]
		valS := pairs[i+1]
		var val uint64
		for _, f := range idxDef.Fields {
			if f.GetName() != field {
				continue
			}
			switch f.GetType() {
			case pdpb.Text:
				doc.Doc.StrProps = append(doc.Doc.StrProps, &cql.StrProp{Name: f.GetName(), Val: valS})
			case pdpb.Uint8:
				if val, err = strconv.ParseUint(valS, 10, 64); err != nil {
					return
				}
				doc.Doc.UintProps = append(doc.Doc.UintProps, &cql.UintProp{Name: f.GetName(), Val: val, ValLen: 1})
			case pdpb.Uint16:
				if val, err = strconv.ParseUint(valS, 10, 64); err != nil {
					return
				}
				doc.Doc.UintProps = append(doc.Doc.UintProps, &cql.UintProp{Name: f.GetName(), Val: val, ValLen: 2})
			case pdpb.Uint32:
				if val, err = strconv.ParseUint(valS, 10, 64); err != nil {
					return
				}
				doc.Doc.UintProps = append(doc.Doc.UintProps, &cql.UintProp{Name: f.GetName(), Val: val, ValLen: 4})
			case pdpb.Uint64:
				if val, err = strconv.ParseUint(valS, 10, 64); err != nil {
					return
				}
				doc.Doc.UintProps = append(doc.Doc.UintProps, &cql.UintProp{Name: f.GetName(), Val: val, ValLen: 8})
			case pdpb.Float32:
				if val, err = util.Float32ToSortableUint64(valS); err != nil {
					return
				}
				doc.Doc.UintProps = append(doc.Doc.UintProps, &cql.UintProp{Name: f.GetName(), Val: val, ValLen: 4, IsFloat: true})
			case pdpb.Float64:
				if val, err = util.Float64ToSortableUint64(valS); err != nil {
					return
				}
				doc.Doc.UintProps = append(doc.Doc.UintProps, &cql.UintProp{Name: f.GetName(), Val: val, ValLen: 8, IsFloat: true})
			default:
				err = errors.Errorf("invalid field type %+v of idxDef %+v", f.GetType().String(), idxDef)
				return
			}
		}
	}
	return
}

//IndicesDiff is indices definion difference
type IndicesDiff struct {
	toDelete []*pdpb.IndexDef
	toCreate []*pdpb.IndexDef
}

//detect difference of indices and indicesNew
func diffIndices(indices, indicesNew map[string]*pdpb.IndexDef) (delta *IndicesDiff) {
	delta = &IndicesDiff{
		toDelete: make([]*pdpb.IndexDef, 0),
		toCreate: make([]*pdpb.IndexDef, 0),
	}
	var ok bool
	var name string
	var idxDefCur, idxDefNew *pdpb.IndexDef
	for name, idxDefCur = range indices {
		if idxDefNew, ok = indicesNew[name]; !ok {
			delta.toDelete = append(delta.toDelete, idxDefCur)
		} else if !reflect.DeepEqual(idxDefCur, idxDefNew) {
			delta.toDelete = append(delta.toDelete, idxDefCur)
			delta.toCreate = append(delta.toCreate, idxDefNew)
		}
	}
	for _, idxDefNew = range indicesNew {
		if _, ok := indices[idxDefNew.GetName()]; !ok {
			delta.toCreate = append(delta.toCreate, idxDefNew)
		}
	}
	return
}
