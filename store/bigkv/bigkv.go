package bigkv

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigtable"
	"github.com/dfuse-io/kvdb/store"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Store struct {
	client *bigtable.Client
	table  *bigtable.Table

	keyPrefix []byte
	tableName string

	batchPut *store.BatchPut
}

func init() {
	store.Register(&store.Registration{
		Name:        "bigkv",
		FactoryFunc: NewStore,
	})
}

// NewStore supports bigkt://project.instance/tableName?createTable=true
func NewStore(dsnString string) (store.KVStore, error) {
	dsn, err := url.Parse(dsnString)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	projInstance := strings.Split(dsn.Host, ".")
	if len(projInstance) != 2 {
		return nil, fmt.Errorf("dsn %q invalid, ensure host component looks like 'project.instance'", dsnString)
	}

	project := projInstance[0]
	instance := projInstance[1]

	optionalTestEnv(project, instance)

	client, err := bigtable.NewClient(ctx, project, instance)
	if err != nil {
		return nil, err
	}

	maxBytesBeforeFlush := uint64(70000000)
	if qMaxBytes := dsn.Query().Get("maxBytesBeforeFlush"); qMaxBytes != "" {
		ms, err := strconv.ParseUint(qMaxBytes, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("dsn: invalid parameter for max-blocks-before-flush, %s", err)
		}
		maxBytesBeforeFlush = ms
	}

	maxSecondsBeforeFlush := uint64(10)
	if qMaxSeconds := dsn.Query().Get("maxSecondsBeforeFlush"); qMaxSeconds != "" {
		ms, err := strconv.ParseUint(qMaxSeconds, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("dsn: invalid parameter for max-blocks-before-flush, %s", err)
		}
		maxSecondsBeforeFlush = ms
	}

	maxRowsBeforeFlush := uint64(0)
	if qMaxRows := dsn.Query().Get("maxRowsBeforeFlush"); qMaxRows != "" {
		mb, err := strconv.ParseUint(qMaxRows, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("dsn: invalid parameter for max-rows-before-flush, %s", err)
		}
		maxRowsBeforeFlush = mb
	}

	s := &Store{
		client:   client,
		batchPut: store.NewBatchPut(int(maxBytesBeforeFlush), int(maxRowsBeforeFlush), time.Duration(maxSecondsBeforeFlush)*time.Second),
	}

	if keyPrefix := dsn.Query().Get("keyPrefix"); keyPrefix != "" {
		keyPrefixBytes, err := hex.DecodeString(keyPrefix)
		if err != nil {
			return nil, fmt.Errorf("decoding keyPrefix as hex: %s", err)
		}
		s.keyPrefix = keyPrefixBytes
	}

	createTable := dsn.Query().Get("createTable") == "true"

	tableName := strings.Trim(dsn.Path, "/")
	s.table = client.Open(tableName)

	if createTable {
		adminClient, err := bigtable.NewAdminClient(ctx, project, instance)
		if err != nil {
			zlog.Error("failed setting up admin client", zap.Error(err))
		}

		if err := adminClient.CreateTable(ctx, tableName); err != nil && !isAlreadyExistsError(err) {
			zlog.Error("failed creating table", zap.String("name", tableName), zap.Error(err))
		}

		if err := adminClient.CreateColumnFamily(ctx, tableName, "kv"); err != nil && !isAlreadyExistsError(err) {
			zlog.Error("failed creating 'kv' family", zap.String("table_name", tableName), zap.Error(err))
		}

		if err := adminClient.SetGCPolicy(ctx, tableName, "kv", bigtable.MaxVersionsPolicy(1)); err != nil {
			zlog.Error("failed applying gc policy to 'kv' family", zap.String("table_name", tableName), zap.Error(err))
		}
	}

	return s, nil
}

func isAlreadyExistsError(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}

	return st.Code() == codes.AlreadyExists
}

func (s *Store) Close() error {
	return s.client.Close()
}

func (s *Store) Put(ctx context.Context, key, value []byte) (err error) {
	s.batchPut.Put(s.withPrefix(key), value)
	if s.batchPut.ShouldFlush() {
		return s.FlushPuts(ctx)
	}

	return nil
}

func (s *Store) FlushPuts(ctx context.Context) error {
	kvs := s.batchPut.GetBatch()
	if len(kvs) == 0 {
		return nil
	}

	keys := make([]string, len(kvs))
	values := make([]*bigtable.Mutation, len(kvs))
	for idx, kv := range kvs {
		keys[idx] = string(kv.Key)
		mut := bigtable.NewMutation()
		mut.Set("kv", "v", bigtable.Now(), kv.Value)
		values[idx] = mut
	}
	errs, err := s.table.ApplyBulk(ctx, keys, values)
	if err != nil {
		return err
	}
	if len(errs) != 0 {
		return fmt.Errorf("apply bulk error: %s", errs)
	}
	s.batchPut.Reset()
	return nil
}

func (s *Store) Get(ctx context.Context, key []byte) (value []byte, err error) {
	row, err := s.table.ReadRow(ctx, string(s.withPrefix(key)), latestCellFilter)
	if err != nil {
		return nil, err
	}
	if len(row) == 0 {
		return nil, store.ErrNotFound
	}

	return row["kv"][0].Value, nil
}

func (s *Store) BatchGet(ctx context.Context, keys [][]byte) *store.Iterator {
	btKeys := make([]string, len(keys))
	for i, key := range keys {
		btKeys[i] = string(key)
	}

	zlog.Debug("batch get", zap.Int("key_count", len(btKeys)))
	opts := []bigtable.ReadOption{latestCellFilter}

	kr := store.NewIterator(ctx)
	go func() {
		err := s.table.ReadRows(ctx, bigtable.RowList(btKeys), func(row bigtable.Row) bool {
			return kr.PushItem(&store.KV{Key: s.withoutPrefix([]byte(row.Key())), Value: row["kv"][0].Value})
		}, opts...)

		if err != nil {
			kr.PushError(err)
			return
		}

		kr.PushFinished()
	}()

	return kr
}

func (s *Store) Scan(ctx context.Context, start, exclusiveEnd []byte, limit int) *store.Iterator {
	startKey := s.withPrefix(start)
	endKey := s.withPrefix(exclusiveEnd)

	sit := store.NewIterator(ctx)

	if len(endKey) == 0 {
		// Act like the other backends
		sit.PushFinished()
		return sit
	}

	zlog.Debug("scanning", zap.Stringer("start", store.Key(startKey)), zap.Stringer("exclusive_end", store.Key(endKey)), zap.Stringer("limit", store.Limit(limit)))
	opts := []bigtable.ReadOption{latestCellFilter}
	if store.Limit(limit).Bounded() {
		opts = append(opts, bigtable.LimitRows(int64(limit)))
	}

	rowRange := bigtable.NewRange(string(startKey), string(endKey))
	go func() {
		err := s.table.ReadRows(ctx, rowRange, func(row bigtable.Row) bool {
			return sit.PushItem(&store.KV{s.withoutPrefix([]byte(row.Key())), row["kv"][0].Value})
		}, opts...)

		if err != nil {
			sit.PushError(err)
			return
		}
		sit.PushFinished()
	}()

	return sit
}

var latestCellOnly = bigtable.LatestNFilter(1)
var latestCellFilter = bigtable.RowFilter(latestCellOnly)

func (s *Store) Prefix(ctx context.Context, prefix []byte, limit int) *store.Iterator {
	sit := store.NewIterator(ctx)
	zlog.Debug("prefix scanning", zap.Stringer("prefix", store.Key(prefix)), zap.Stringer("limit", store.Limit(limit)))
	opts := []bigtable.ReadOption{latestCellFilter}
	if store.Limit(limit).Bounded() {
		opts = append(opts, bigtable.LimitRows(int64(limit)))
	}

	prefix = s.withPrefix(prefix)

	go func() {
		err := s.table.ReadRows(ctx, bigtable.PrefixRange(string(prefix)), func(row bigtable.Row) bool {
			return sit.PushItem(&store.KV{s.withoutPrefix([]byte(row.Key())), row["kv"][0].Value})
		}, opts...)

		if err != nil {
			sit.PushError(err)
			return
		}

		sit.PushFinished() // there was an error there!
	}()

	return sit
}

func (s *Store) withPrefix(key []byte) []byte {
	if len(s.keyPrefix) == 0 {
		return key
	}
	out := make([]byte, len(s.keyPrefix)+len(key))
	copy(out[0:], s.keyPrefix)
	copy(out[len(s.keyPrefix):], key)
	return out
}

func (s *Store) withoutPrefix(key []byte) []byte {
	if len(s.keyPrefix) == 0 {
		return key
	}
	return key[len(s.keyPrefix):]
}

func optionalTestEnv(project, instance string) {
	if isTestEnv(project, instance) && (os.Getenv(emulatorHostDefault) == "") {
		os.Setenv(emulatorHostDefault, emulatorDefaultHostValue)
	}
}

func isTestEnv(project, instance string) bool {
	return (strings.HasPrefix(project, "dev") || strings.HasPrefix(instance, "dev"))
}

const emulatorHostDefault = "BIGTABLE_EMULATOR_HOST"
const emulatorDefaultHostValue = "localhost:8086"
