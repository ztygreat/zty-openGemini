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

package meta

import (
	"github.com/gogo/protobuf/proto"
	"github.com/openGemini/openGemini/open_src/influx/influxql"
	proto2 "github.com/openGemini/openGemini/open_src/influx/meta/proto"
	"github.com/openGemini/openGemini/open_src/vm/protoparser/influx"
)

type KeyInfo struct {
	ID   uint64 // unique key id
	Ref  int32  // IndexGroupInfo ref count
	Type int32  // data type
}

func (ki KeyInfo) marshal() *proto2.KeyInfo {
	pb := &proto2.KeyInfo{
		ID:   proto.Uint64(ki.ID),
		Ref:  proto.Int32(ki.Ref),
		Type: proto.Int32(ki.Type),
	}
	return pb
}

func (ki *KeyInfo) unmarshal(pb *proto2.KeyInfo) {
	ki.ID = pb.GetID()
	ki.Ref = pb.GetRef()
	ki.Type = pb.GetType()

}

type MeasurementInfo struct {
	Name       string // measurement name with version
	originName string // cache original measurement name
	ShardKeys  []ShardKeyInfo
	// Schema     map[string]int32
	Schema        map[string]KeyInfo // tags/fields
	IndexRelation IndexRelation
	MarkDeleted   bool
}

func NewMeasurementInfo(nameWithVer string) *MeasurementInfo {
	return &MeasurementInfo{
		Name:       nameWithVer,
		originName: influx.GetOriginMstName(nameWithVer),
	}
}

func (msti *MeasurementInfo) OriginName() string {
	return msti.originName
}

func (msti *MeasurementInfo) walkSchema(fn func(fieldName string, fieldType int32)) {
	for fieldName := range msti.Schema {
		fn(fieldName, msti.Schema[fieldName].Type)
	}
}

func (msti *MeasurementInfo) GetShardKey(ID uint64) *ShardKeyInfo {
	for i := len(msti.ShardKeys) - 1; i >= 0; i-- {
		if msti.ShardKeys[i].ShardGroup <= ID {
			return &msti.ShardKeys[i]
		}
	}
	return nil
}

func (msti *MeasurementInfo) marshal() *proto2.MeasurementInfo {
	pb := &proto2.MeasurementInfo{
		Name:        proto.String(msti.Name),
		MarkDeleted: proto.Bool(msti.MarkDeleted),
	}

	if msti.ShardKeys != nil {
		pb.ShardKeys = make([]*proto2.ShardKeyInfo, len(msti.ShardKeys))
		for i := range msti.ShardKeys {
			pb.ShardKeys[i] = msti.ShardKeys[i].Marshal()
		}
	}

	if msti.Schema != nil {
		pb.Schema = make(map[string]*proto2.KeyInfo, len(msti.Schema))
		for n, t := range msti.Schema {
			pb.Schema[n] = t.marshal()
		}
	}

	pb.IndexRelation = msti.IndexRelation.Marshal()
	return pb
}

func (msti *MeasurementInfo) unmarshal(pb *proto2.MeasurementInfo) {
	msti.Name = pb.GetName()
	msti.originName = influx.GetOriginMstName(msti.Name)
	msti.MarkDeleted = pb.GetMarkDeleted()
	if pb.GetShardKeys() != nil {
		msti.ShardKeys = make([]ShardKeyInfo, len(pb.GetShardKeys()))
		for i := range pb.GetShardKeys() {
			msti.ShardKeys[i].unmarshal(pb.GetShardKeys()[i])
		}
	}

	// if len(pb.GetSchema()) > 0 {
	// 	msti.Schema = make(map[string]KeyInfo, len(pb.GetSchema()))
	// }
	if len(pb.GetSchema()) > 0 {
		msti.Schema = make(map[string]KeyInfo, len(pb.GetSchema()))
		for name, t := range pb.GetSchema() {
			ki := &KeyInfo{}
			ki.unmarshal(t)
			msti.Schema[name] = *ki
		}
	}

	msti.IndexRelation.unmarshal(pb.GetIndexRelation())
}

func (msti *MeasurementInfo) MarshalBinary() ([]byte, error) {
	pb := msti.marshal()
	return proto.Marshal(pb)
}

func (msti *MeasurementInfo) UnmarshalBinary(buf []byte) error {
	pb := &proto2.MeasurementInfo{}
	if err := proto.Unmarshal(buf, pb); err != nil {
		return err
	}
	msti.unmarshal(pb)
	return nil
}

func (msti MeasurementInfo) clone() *MeasurementInfo {
	other := msti
	other.Schema = msti.cloneSchema()
	if msti.ShardKeys == nil {
		return &other
	}
	other.ShardKeys = make([]ShardKeyInfo, len(msti.ShardKeys))
	for i := range msti.ShardKeys {
		other.ShardKeys[i] = msti.ShardKeys[i].clone()
	}

	return &other
}

func (msti MeasurementInfo) cloneSchema() map[string]KeyInfo {
	if msti.Schema == nil {
		return nil
	}

	schema := make(map[string]KeyInfo, len(msti.Schema))
	for name, info := range msti.Schema {
		schema[name] = info
	}
	return schema
}

func (msti MeasurementInfo) FieldKeys(ret map[string]map[string]int32) {
	for key := range msti.Schema {
		if msti.Schema[key].Type == influx.Field_Type_Tag {
			continue
		}
		ret[msti.OriginName()][key] = msti.Schema[key].Type
	}
}

func (msti MeasurementInfo) MatchTagKeys(cond influxql.Expr, ret map[string]map[string]struct{}) {
	for key, inf := range msti.Schema {
		if inf.Type != influx.Field_Type_Tag {
			continue
		}
		valMap := map[string]interface{}{
			"_tagKey": key,
			"_name":   msti.OriginName(),
		}
		if cond == nil || influxql.EvalBool(cond, valMap) {
			ret[msti.Name][key] = struct{}{}
		}
	}
}

type ShardKeyInfo struct {
	ShardKey   []string
	Type       string
	ShardGroup uint64
}

func (ski *ShardKeyInfo) EqualsToAnother(other *ShardKeyInfo) bool {
	if len(ski.ShardKey) != len(other.ShardKey) {
		return false
	}

	if ski.Type != other.Type {
		return false
	}

	// shardKey is sorted
	for i := range ski.ShardKey {
		if ski.ShardKey[i] != other.ShardKey[i] {
			return false
		}
	}

	return true
}

func (ski *ShardKeyInfo) Marshal() *proto2.ShardKeyInfo {
	pb := &proto2.ShardKeyInfo{ShardKey: ski.ShardKey, Type: proto.String(ski.Type)}
	if ski.ShardGroup > 0 {
		pb.SgID = proto.Uint64(ski.ShardGroup)
	}
	return pb
}

func (ski *ShardKeyInfo) unmarshal(pb *proto2.ShardKeyInfo) {
	ski.ShardKey = pb.GetShardKey()
	ski.Type = pb.GetType()
	if pb.GetSgID() > 0 {
		ski.ShardGroup = pb.GetSgID()
	}
}

func (ski ShardKeyInfo) clone() ShardKeyInfo {
	if ski.ShardKey == nil {
		return ski
	}

	shardKey := make([]string, len(ski.ShardKey))
	for i := range ski.ShardKey {
		shardKey[i] = ski.ShardKey[i]
	}

	ski.ShardKey = shardKey
	return ski
}

type IndexRelation struct {
	Rid        uint32
	Oids       []uint32
	IndexNames []string
	IndexList  []*IndexList
}

type IndexList struct {
	IList []string
}

func (indexR *IndexRelation) Marshal() *proto2.IndexRelation {
	pb := &proto2.IndexRelation{Rid: proto.Uint32(indexR.Rid),
		Oid:       indexR.Oids,
		IndexName: indexR.IndexNames}

	pb.IndexLists = make([]*proto2.IndexList, len(indexR.IndexList))
	for i, IList := range indexR.IndexList {
		indexList := &proto2.IndexList{
			IList: IList.IList,
		}
		pb.IndexLists[i] = indexList
	}
	return pb
}

func (indexR *IndexRelation) unmarshal(pb *proto2.IndexRelation) {
	indexR.Rid = pb.GetRid()
	indexR.Oids = pb.GetOid()
	indexR.IndexNames = pb.GetIndexName()
	indexLists := pb.GetIndexLists()
	indexR.IndexList = make([]*IndexList, len(indexLists))
	for i, iList := range indexLists {
		indexR.IndexList[i] = &IndexList{
			IList: iList.GetIList(),
		}
	}
}

func (msti *MeasurementInfo) ContainIndexRelation(ID uint64) bool {
	return true
}

func (msti *MeasurementInfo) GetIndexRelation() IndexRelation {
	return msti.IndexRelation
}

func (msti *MeasurementInfo) FindMstInfos(dataTypes []int64) []*MeasurementTypeFields {
	infos := make([]*MeasurementTypeFields, 0, len(dataTypes))
	for _, d := range dataTypes {
		info := &MeasurementTypeFields{
			Fields: make([]string, 0),
		}
		switch influxql.DataType(d) {
		case influxql.Float:
			info.Type = int64(influxql.Float)
			for name, inf := range msti.Schema {
				if inf.Type == influx.Field_Type_Float {
					info.Fields = append(info.Fields, name)
				}
			}
		case influxql.Integer:
			info.Type = int64(influxql.Integer)
			for name, inf := range msti.Schema {
				if inf.Type == influx.Field_Type_Int {
					info.Fields = append(info.Fields, name)
				}
			}
		case influxql.String:
			info.Type = int64(influxql.String)
			for name, inf := range msti.Schema {
				if inf.Type == influx.Field_Type_String {
					info.Fields = append(info.Fields, name)
				}
			}
		case influxql.Boolean:
			info.Type = int64(influxql.Boolean)
			for name, inf := range msti.Schema {
				if inf.Type == influx.Field_Type_Boolean {
					info.Fields = append(info.Fields, name)
				}
			}
		}
		if len(info.Fields) > 0 {
			infos = append(infos, info)
		}
	}
	return infos
}
