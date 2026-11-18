package dbx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type aiMongoFind struct {
	Collection string          `json:"collection"`
	Filter     json.RawMessage `json:"filter,omitempty"`
	Skip       int64           `json:"skip,omitempty"`
}

type aiMongoWrite struct {
	Operation  string          `json:"operation"`
	Collection string          `json:"collection"`
	Filter     json.RawMessage `json:"filter,omitempty"`
	Document   json.RawMessage `json:"document,omitempty"`
	Changes    json.RawMessage `json:"changes,omitempty"`
	To         string          `json:"to,omitempty"`
}

func decodeMongoAI(src string, into any) error {
	if len(src) == 0 || len(src) > aiMaxSQL || !json.Valid([]byte(src)) || !strings.HasPrefix(strings.TrimSpace(src), "{") {
		return errors.New("MongoDB 操作须是一个 Extended JSON 对象")
	}
	d := json.NewDecoder(strings.NewReader(src))
	d.DisallowUnknownFields()
	return d.Decode(into)
}

func checkMongoNamespace(db, collection string) error {
	switch strings.ToLower(db) {
	case "", "admin", "config", "local":
		return errors.New("请指定 MongoDB 的业务数据库")
	}
	if collection == "" || strings.HasPrefix(strings.ToLower(collection), "system.") || strings.ContainsAny(collection, "$\x00") {
		return errors.New("请指定 MongoDB 的业务集合")
	}
	return nil
}

func safeMongoFilter(value any) bool {
	switch v := value.(type) {
	case bson.D:
		for _, e := range v {
			switch strings.ToLower(e.Key) {
			case "$where", "$function", "$accumulator":
				return false
			}
			if !safeMongoFilter(e.Value) {
				return false
			}
		}
	case bson.A:
		for _, e := range v {
			if !safeMongoFilter(e) {
				return false
			}
		}
	case bson.JavaScript, bson.CodeWithScope:
		return false
	}
	return true
}

func mongoAIFilter(src json.RawMessage) (bson.D, error) {
	f, err := mongoFilter(string(src))
	if err == nil && !safeMongoFilter(f) {
		err = errors.New("MongoDB 的 AI 筛选条件不能执行 JavaScript")
	}
	return f, err
}

func parseAIMongoFind(db, src string) (aiMongoFind, error) {
	var in aiMongoFind
	if err := decodeMongoAI(src, &in); err != nil {
		return in, err
	}
	if err := checkMongoNamespace(db, in.Collection); err != nil {
		return in, err
	}
	if in.Skip < 0 {
		return in, errors.New("MongoDB skip 不能小于零")
	}
	_, err := mongoAIFilter(in.Filter)
	return in, err
}

func parseAIMongoWrite(db, src string) (aiMongoWrite, error) {
	var in aiMongoWrite
	if err := decodeMongoAI(src, &in); err != nil {
		return in, err
	}
	if err := checkMongoNamespace(db, in.Collection); err != nil {
		return in, err
	}
	switch in.Operation {
	case "insertOne":
		if len(in.Document) == 0 {
			return in, errors.New("insertOne 需要 document")
		}
		_, err := mongoFilter(string(in.Document))
		return in, err
	case "updateMany", "deleteMany":
		if len(in.Filter) == 0 {
			return in, errors.New("需要写明 filter，改动整个集合时填 {}")
		}
		if _, err := mongoAIFilter(in.Filter); err != nil {
			return in, err
		}
		if in.Operation == "updateMany" {
			d, err := mongoFilter(string(in.Changes))
			if err != nil {
				return in, err
			}
			if len(d) == 0 {
				return in, errors.New("updateMany 需要 changes 对象")
			}
			for _, e := range d {
				if strings.HasPrefix(e.Key, "$") {
					return in, errors.New("changes 中只填写要设置的字段和值")
				}
			}
		}
	case "renameCollection":
		return in, checkMongoNamespace(db, in.To)
	case "createCollection", "dropCollection":
	default:
		return in, errors.New("MongoDB 支持 insertOne、updateMany、deleteMany、createCollection、renameCollection、dropCollection")
	}
	return in, nil
}

func aiMongoDanger(stmts []string) string {
	if len(stmts) == 1 {
		var in aiMongoWrite
		if decodeMongoAI(stmts[0], &in) == nil {
			f, _ := mongoAIFilter(in.Filter)
			switch {
			case in.Operation == "dropCollection":
				return "会删除集合和里面的所有文档"
			case (in.Operation == "deleteMany" || in.Operation == "updateMany") && len(f) == 0:
				return "筛选条件为空，会改动集合中的所有文档"
			}
		}
	}
	return ""
}

func (cl *Client) aiMongoQuery(ctx context.Context, db, src string, limit int) (*AIResult, error) {
	db = cl.mongoDatabaseName(db)
	in, err := parseAIMongoFind(db, src)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = AIDefaultRows
	}
	limit = min(AIMaxRows, limit)
	ctx, cancel := context.WithTimeout(ctx, AITimeout)
	defer cancel()
	start := time.Now()
	cursor, err := cl.mongoFind(ctx, db, in.Collection, string(in.Filter), in.Skip, int64(limit+1))
	if err != nil {
		return nil, err
	}
	defer closeMongoCursor(cursor)
	r := &AIResult{Columns: []Column{{Name: "document", Type: "Extended JSON"}}, Rows: [][]any{}}
	size := 0
	for cursor.Next(ctx) {
		data, err := bson.MarshalExtJSON(cursor.Current, true, false)
		if err != nil {
			return nil, err
		}
		doc := aiCell(string(data)).(string)
		size += len(doc)
		if len(r.Rows) >= limit || size > aiMaxBytes {
			r.More = true
			r.Note = "结果达到行数或长度上限，可以用 filter、skip 继续查询"
			break
		}
		r.Rows = append(r.Rows, []any{doc})
	}
	r.Ms = time.Since(start).Milliseconds()
	return r, cl.explain(cursor.Err())
}

func (cl *Client) MongoAICollection(ctx context.Context, db, collection string) (any, error) {
	db = cl.mongoDatabaseName(db)
	if err := checkMongoNamespace(db, collection); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, AITimeout)
	defer cancel()
	indexes, err := cl.mongo.Database(db).Collection(collection).Indexes().ListSpecifications(ctx)
	if err != nil {
		return nil, cl.explain(err)
	}
	return map[string]any{"collection": collection, "database": db, "indexes": indexes}, nil
}

func (cl *Client) aiMongoExec(ctx context.Context, db, src string) (*AIExecResult, error) {
	db = cl.mongoDatabaseName(db)
	in, err := parseAIMongoWrite(db, src)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, AIWriteTimeout)
	defer cancel()
	start := time.Now()
	var n int64
	transactional := cl.mongoTransactions && (in.Operation == "updateMany" || in.Operation == "deleteMany")
	if transactional {
		session, err := cl.mongo.StartSession()
		if err != nil {
			return nil, cl.explain(err)
		}
		defer func() {
			done, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			session.EndSession(done)
		}()
		_, err = session.WithTransaction(ctx, func(tx context.Context) (any, error) {
			n, err = cl.runMongoWrite(tx, db, in)
			return nil, err
		})
		if err != nil {
			r := mongoWriteResult(start, src, n, cl.explain(err))
			r.Note = "MongoDB 事务失败，未确认提交"
			return r, nil
		}
	} else {
		n, err = cl.runMongoWrite(ctx, db, in)
	}
	return mongoWriteResult(start, src, n, cl.explain(err)), nil
}

func (cl *Client) runMongoWrite(ctx context.Context, db string, in aiMongoWrite) (int64, error) {
	database := cl.mongo.Database(db)
	collection := database.Collection(in.Collection)
	switch in.Operation {
	case "insertOne":
		doc, _ := mongoFilter(string(in.Document))
		_, err := collection.InsertOne(ctx, doc)
		return 1, err
	case "updateMany":
		filter, _ := mongoAIFilter(in.Filter)
		changes, _ := mongoFilter(string(in.Changes))
		result, err := collection.UpdateMany(ctx, filter, bson.D{{Key: "$set", Value: changes}})
		if result != nil {
			return result.ModifiedCount, err
		}
		return 0, err
	case "deleteMany":
		filter, _ := mongoAIFilter(in.Filter)
		result, err := collection.DeleteMany(ctx, filter)
		if result != nil {
			return result.DeletedCount, err
		}
		return 0, err
	case "createCollection":
		return 0, database.CreateCollection(ctx, in.Collection)
	case "renameCollection":
		return 0, cl.mongo.Database("admin").RunCommand(ctx, bson.D{
			{Key: "renameCollection", Value: db + "." + in.Collection}, {Key: "to", Value: db + "." + in.To},
		}).Err()
	case "dropCollection":
		return 0, collection.Drop(ctx)
	}
	return 0, errors.New("不支持的 MongoDB 操作")
}

func mongoWriteResult(start time.Time, src string, n int64, err error) *AIExecResult {
	r := &AIExecResult{OK: err == nil, Committed: err == nil, Ms: time.Since(start).Milliseconds()}
	one := AIStmtResult{SQL: src, Ms: r.Ms}
	if err != nil {
		one.Error = err.Error()
		r.Note = "MongoDB 操作失败，部分写入可能已生效"
	} else {
		one.Affected = &n
	}
	r.Results = []AIStmtResult{one}
	return r
}
