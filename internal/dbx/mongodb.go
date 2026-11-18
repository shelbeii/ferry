package dbx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (cl *Client) openMongo(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout+5*time.Second)
	defer cancel()
	cfg := options.Client().SetHosts([]string{cl.addr}).SetAppName("Ferry").
		SetConnectTimeout(dialTimeout).SetServerSelectionTimeout(dialTimeout).
		SetMaxPoolSize(4).SetMaxConnIdleTime(5 * time.Minute)
	if cl.Conn.Direct || cl.Conn.Container != "" {
		cfg.SetDirect(true)
	}
	if cl.tn != nil {
		// 副本集报告的其他节点由 SSH 服务器解析并连接。
		cfg.SetDialer(dbDialer{cl})
	}
	if cl.Conn.User != "" {
		source := cl.Conn.AuthSource
		if source == "" {
			source = "admin"
		}
		cfg.SetAuth(options.Credential{Username: cl.Conn.User, Password: cl.password, PasswordSet: true, AuthSource: source})
	} else if cl.password != "" {
		return errors.New("使用 MongoDB 密码时请填写用户名")
	}
	if mode := cl.tlsMode(); mode != TLSOff {
		tls := cl.tlsConfig()
		// 由驱动按实际连接的副本集成员校验证书主机名。
		tls.ServerName = ""
		cfg.SetTLSConfig(tls)
		cl.Encrypted = true
	}
	client, err := mongo.Connect(cfg)
	if err != nil {
		return cl.explain(err)
	}
	cl.mongo = client
	if err := client.Ping(ctx, nil); err != nil {
		return cl.explain(err)
	}
	cl.Version = "MongoDB"
	var info struct{ Version string }
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err == nil && info.Version != "" {
		cl.Version += " " + info.Version
	}
	if cl.Conn.AI && cl.Conn.AIWrite && !cl.Conn.ReadOnly {
		var topology struct {
			SetName string `bson:"setName"`
			Msg     string
		}
		if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&topology); err == nil {
			cl.mongoTransactions = topology.SetName != "" || topology.Msg == "isdbgrid"
		}
	}
	return nil
}

func mongoAuthFailed(err error) bool {
	var e mongo.ServerError
	if errors.As(err, &e) && e.HasErrorCode(18) {
		return true
	}
	// 认证发生在连接握手时，驱动会把认证错误连同服务器选择错误返回。
	return strings.Contains(err.Error(), "(AuthenticationFailed)")
}

func (cl *Client) mongoDatabases(ctx context.Context) ([]Database, error) {
	names, err := cl.mongo.ListDatabaseNames(ctx, bson.D{}, options.ListDatabases().SetAuthorizedDatabases(true))
	if err != nil {
		return nil, cl.explain(err)
	}
	sort.Strings(names)
	out := make([]Database, 0, len(names))
	for _, name := range names {
		out = append(out, Database{Name: name, System: name == "admin" || name == "config" || name == "local"})
	}
	return out, nil
}

func (cl *Client) mongoCollections(ctx context.Context, dbName string) ([]Table, error) {
	dbName = cl.mongoDatabaseName(dbName)
	if dbName == "" {
		return nil, errors.New("请指定 MongoDB 数据库")
	}
	specs, err := cl.mongo.Database(dbName).ListCollectionSpecifications(ctx, bson.D{},
		options.ListCollections().SetNameOnly(true).SetAuthorizedCollections(true))
	if err != nil {
		return nil, cl.explain(err)
	}
	out := make([]Table, 0, len(specs))
	for _, s := range specs {
		kind := "table"
		if s.Type == "view" {
			kind = "view"
		}
		out = append(out, Table{Name: s.Name, Type: kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (cl *Client) mongoDatabaseName(dbName string) string {
	if dbName != "" {
		return dbName
	}
	return cl.Conn.Database
}

type MongoFindOptions struct {
	Filter string `json:"filter,omitempty"` // Extended JSON 对象
	Skip   int64  `json:"skip,omitempty"`
	Limit  int64  `json:"limit,omitempty"`
}

type MongoDocument struct {
	JSON string `json:"json"`
	Cut  bool   `json:"cut,omitempty"`
}

type MongoResult struct {
	Documents []MongoDocument `json:"documents"`
	More      bool            `json:"more"`
}

func mongoFilter(src string) (bson.D, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return bson.D{}, nil
	}
	var filter bson.D
	data := []byte(src)
	if src[0] != '{' || !json.Valid(data) || bson.UnmarshalExtJSON(data, false, &filter) != nil {
		return nil, errors.New("MongoDB 筛选条件须是一个合法的 Extended JSON 对象")
	}
	return filter, nil
}

func (cl *Client) mongoFind(ctx context.Context, db, collection, filter string, skip, limit int64) (*mongo.Cursor, error) {
	if cl.Conn.Kind != MongoDB {
		return nil, errors.New("此操作仅支持 MongoDB")
	}
	db = cl.mongoDatabaseName(db)
	if db == "" || collection == "" {
		return nil, errors.New("请指定 MongoDB 数据库和集合")
	}
	f, err := mongoFilter(filter)
	if err != nil {
		return nil, err
	}
	o := options.Find().SetSort(bson.D{{Key: "_id", Value: 1}})
	if skip > 0 {
		o.SetSkip(skip)
	}
	if limit > 0 {
		o.SetLimit(limit)
	}
	cursor, err := cl.mongo.Database(db).Collection(collection).Find(ctx, f, o)
	return cursor, cl.explain(err)
}

func closeMongoCursor(cursor *mongo.Cursor) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = cursor.Close(ctx)
}

func (cl *Client) MongoDocuments(ctx context.Context, db, collection string, o MongoFindOptions) (*MongoResult, error) {
	if o.Limit <= 0 || o.Limit > 100 {
		o.Limit = 25
	}
	cursor, err := cl.mongoFind(ctx, db, collection, o.Filter, o.Skip, o.Limit+1)
	if err != nil {
		return nil, err
	}
	defer closeMongoCursor(cursor)
	out := &MongoResult{Documents: []MongoDocument{}}
	size := 0
	for cursor.Next(ctx) {
		if int64(len(out.Documents)) >= o.Limit || size >= maxResultBytes {
			out.More = true
			break
		}
		data, err := bson.MarshalExtJSONIndent(cursor.Current, true, false, "", "  ")
		if err != nil {
			return nil, err
		}
		cut := len(data) > MaxCell
		if cut {
			data = data[:MaxCell]
			for !utf8.Valid(data) {
				data = data[:len(data)-1]
			}
		}
		out.Documents = append(out.Documents, MongoDocument{JSON: string(data), Cut: cut})
		size += len(data)
	}
	return out, cl.explain(cursor.Err())
}

func (cl *Client) MongoExport(ctx context.Context, db, collection, filter, path string, progress func(int64)) (*ExportResult, error) {
	start := time.Now()
	cursor, err := cl.mongoFind(ctx, db, collection, filter, 0, 0)
	if err != nil {
		return nil, err
	}
	defer closeMongoCursor(cursor)
	f, err := createOut(path)
	if err != nil {
		return nil, err
	}
	defer f.abort()
	n, err := writeMongoJSON(ctx, f.w, cursor, progress)
	if err != nil {
		return nil, cl.explain(err)
	}
	size, err := f.finish()
	if err != nil {
		return nil, err
	}
	return &ExportResult{Rows: n, Bytes: size, Ms: time.Since(start).Milliseconds(), Out: path}, nil
}

func writeMongoJSON(ctx context.Context, w io.Writer, cursor *mongo.Cursor, progress func(int64)) (int64, error) {
	if _, err := io.WriteString(w, "["); err != nil {
		return 0, err
	}
	var n int64
	for cursor.Next(ctx) {
		separator := "\n"
		if n > 0 {
			separator = ",\n"
		}
		if _, err := io.WriteString(w, separator); err != nil {
			return n, err
		}
		j, err := bson.MarshalExtJSON(cursor.Current, true, false)
		if err != nil {
			return n, err
		}
		if _, err := w.Write(j); err != nil {
			return n, err
		}
		n++
		if progress != nil && n%1000 == 0 {
			progress(n)
		}
	}
	if err := cursor.Err(); err != nil {
		return n, err
	}
	if err := ctx.Err(); err != nil {
		return n, err
	}
	_, err := io.WriteString(w, "\n]\n")
	return n, err
}
