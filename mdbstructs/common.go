package mdbstructs

import (
	"time"

	"github.com/globalsign/mgo/bson"
)

type ClusterTime struct {
	ClusterTime time.Time `bson:"clusterTime"`
	Signature   struct {
		Hash  bson.Binary `bson:"hash"`
		KeyID int64       `bson:"keyId"`
	} `bson:"signature"`
}

type ConfigServerState struct {
	OpTime OpTime `bson:"opTime"`
}

type GleStats struct {
	LastOpTime bson.MongoTimestamp `bson:"lastOpTime"`
	ElectionId bson.ObjectId       `bson:"electionId"`
}

type OpTime struct {
	Ts   bson.MongoTimestamp `bson:"ts" json:"ts"`
	Term int64               `bson:"t" json:"t"`
}
