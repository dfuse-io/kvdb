package storetest

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/dfuse-io/kvdb/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type kvStoreOptions struct {
	withPurgeable             bool
	purgeableStoreTablePrefix []byte
	purgeableTTLInBlocks      uint64
}

var kvstoreTests = []struct {
	name    string
	test    func(t *testing.T, driver store.KVStore, options kvStoreOptions)
	options kvStoreOptions
}{
	{
		name: "basic",
		test: TestBasic,
		options: kvStoreOptions{
			withPurgeable: false,
		},
	},
	{
		name: "purgeable",
		test: TestPurgeable,
		options: kvStoreOptions{
			withPurgeable:             true,
			purgeableStoreTablePrefix: []byte{0x09},
			purgeableTTLInBlocks:      1,
		},
	},
}

func TestAllKVStore(t *testing.T, driverName string, driverFactory DriverFactory, testPurgeableStore bool) {
	for _, test := range kvstoreTests {
		testName := driverName + "/" + test.name
		t.Run(testName, func(t *testing.T) {
			opts := []store.Option{}
			if test.options.withPurgeable {
				if !testPurgeableStore {
					t.Skipf("Unable to test purgeable for driver %s must enable it", testName)
					return
				}
				opts = append(opts, store.WithPurgeableStore(test.options.purgeableStoreTablePrefix, test.options.purgeableTTLInBlocks))
			}
			driver, closer := driverFactory(opts...)
			defer closer()
			test.test(t, driver, test.options)
		})
	}
}

func TestPurgeable(t *testing.T, driver store.KVStore, options kvStoreOptions) {
	tests := []struct {
		key    []byte
		value  []byte
		height uint64
	}{
		{
			key:    []byte("a"),
			value:  []byte("1"),
			height: 90,
		},
		{
			key:    []byte("ba"),
			value:  []byte("2"),
			height: 80,
		},
		{
			key:    []byte("ba1"),
			value:  []byte("3"),
			height: 92,
		},
		{
			key:    []byte("ba2"),
			value:  []byte("4"),
			height: 94,
		},
		{
			key:    []byte("bb"),
			value:  []byte("5"),
			height: 1085,
		},
		{
			key:    []byte("c"),
			value:  []byte("6"),
			height: 96,
		},
	}

	var ephemeralDriver *store.PurgeableKVStore
	var ok bool
	if ephemeralDriver, ok = driver.(*store.PurgeableKVStore); !ok {
		t.Fatalf("expected a purgeable kvstore to run the test. Ensure that you enable `withPurgeable` to true")
		return
	}

	// Putting the keys in DB
	for _, test := range tests {
		ephemeralDriver.MarkCurrentHeight(test.height)
		err := ephemeralDriver.Put(context.Background(), test.key, test.value)
		require.NoError(t, err)
	}

	// testing Flush Put
	err := driver.FlushPuts(context.Background())
	require.NoError(t, err)

	// Ensuring that the Keys are in the DB
	for _, test := range tests {
		// testing GET function
		v, err := driver.Get(context.Background(), test.key)
		require.NoError(t, err)
		require.Equal(t, test.value, v)
	}

	// Ensuring that the deletions Keys are in the DB
	for _, test := range tests {
		// testing GET function
		expectedDeletionKey := testDeleteKeyGenerate(t, options.purgeableStoreTablePrefix, test.height, test.key)
		v, err := driver.Get(context.Background(), expectedDeletionKey)
		require.NoError(t, err)
		require.Equal(t, []byte{0x00}, v)
	}

	// testing Purge
	purgeBelowHeight := uint64(92)
	ephemeralDriver.MarkCurrentHeight(purgeBelowHeight)
	err = ephemeralDriver.PurgeKeys(context.Background())
	require.NoError(t, err)

	// Ensuring that the Keys have been purged correctly
	for _, test := range tests {
		// testing GET function
		v, err := driver.Get(context.Background(), test.key)
		if test.height < (purgeBelowHeight - options.purgeableTTLInBlocks) {
			require.Error(t, err)
			assert.Equal(t, err, store.ErrNotFound)
		} else {
			require.NoError(t, err)
			require.Equal(t, test.value, v)
		}
	}

	// Ensuring that the Deletion Keys have been purged correctly
	for _, test := range tests {
		// testing GET function
		v, err := driver.Get(context.Background(), test.key)
		if test.height < (purgeBelowHeight - options.purgeableTTLInBlocks) {
			require.Error(t, err)
			assert.Equal(t, err, store.ErrNotFound)
		} else {
			require.NoError(t, err)
			require.Equal(t, test.value, v)
		}
	}

}

func TestBasic(t *testing.T, driver store.KVStore, options kvStoreOptions) {
	all := []store.KV{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("ba"), Value: []byte("2")},
		{Key: []byte("ba1"), Value: []byte("3")},
		{Key: []byte("ba2"), Value: []byte("4")},
		{Key: []byte("bb"), Value: []byte("5")},
		{Key: []byte("c"), Value: []byte("6")},
	}

	// testing PUT function
	for _, kv := range all {
		err := driver.Put(context.Background(), kv.Key, kv.Value)
		require.NoError(t, err)
	}

	// testing GET without a flush  //// SKIP: Some backends still flush.
	// for _, kv := range all {
	// 	// testing GET function
	// 	_, err := driver.Get(context.Background(), kv.Key)
	// 	require.Equal(t, kvdb.ErrNotFound, err)
	// }

	// testing Flush Put
	err := driver.FlushPuts(context.Background())
	require.NoError(t, err)

	// testing GET with a flush
	for _, kv := range all {
		// testing GET function
		v, err := driver.Get(context.Background(), kv.Key)
		require.NoError(t, err)
		require.Equal(t, kv.Value, v)
	}

	// None existant key
	_, err = driver.Get(context.Background(), []byte("keydoesnotexists"))
	require.Equal(t, store.ErrNotFound, err)

	// testing Prefix without limit
	testPrefix(t, driver, nil, store.Unlimited, all)
	testPrefix(t, driver, []byte("a"), store.Unlimited, all[:1])
	testPrefix(t, driver, []byte("c"), store.Unlimited, all[5:])
	testPrefix(t, driver, []byte("b"), store.Unlimited, all[1:5])
	testPrefix(t, driver, []byte("ba"), store.Unlimited, all[1:4])

	// testing Prefix with limit
	testPrefix(t, driver, nil, 2, all[:2])
	testPrefix(t, driver, nil, 5, all[:5])
	testPrefix(t, driver, nil, 10, all)

	testPrefix(t, driver, []byte("a"), 2, all[:1])
	testPrefix(t, driver, []byte("c"), 1, all[5:6])
	testPrefix(t, driver, []byte("b"), 3, all[1:4])
	testPrefix(t, driver, []byte("ba"), 10, all[1:4])

	// testing BatchPrefix without limit
	testBatchPrefix(t, driver, [][]byte{[]byte("ba")}, store.Unlimited, all[1:4]...)
	testBatchPrefix(t, driver, [][]byte{[]byte("ba"), []byte("c")}, store.Unlimited, all[1], all[2], all[3], all[5])
	testBatchPrefix(t, driver, [][]byte{[]byte("a"), []byte("c")}, store.Unlimited, all[0], all[5])
	testBatchPrefix(t, driver, [][]byte{[]byte("d"), []byte("f")}, store.Unlimited)

	// testing BatchPrefix with limit
	testBatchPrefix(t, driver, [][]byte{[]byte("ba")}, 1, all[1])
	testBatchPrefix(t, driver, [][]byte{[]byte("ba"), []byte("c")}, 2, all[1], all[2])
	testBatchPrefix(t, driver, [][]byte{[]byte("a"), []byte("c")}, 1, all[0])
	testBatchPrefix(t, driver, [][]byte{[]byte("d"), []byte("f")}, 10)

	// testing Scan without limit
	testScan(t, driver, []byte("a"), []byte("a"), store.Unlimited, nil)
	testScan(t, driver, []byte("a"), []byte("b"), store.Unlimited, all[:1])
	testScan(t, driver, []byte("b"), []byte("a"), store.Unlimited, nil)
	testScan(t, driver, []byte("b"), []byte("bb"), store.Unlimited, all[1:4])
	testScan(t, driver, []byte("b"), []byte("c"), store.Unlimited, all[1:5])
	testScan(t, driver, []byte("a"), []byte("c"), store.Unlimited, all[:5])
	testScan(t, driver, []byte("ba"), []byte("bb"), store.Unlimited, all[1:4])
	testScan(t, driver, nil, nil, store.Unlimited, nil)
	testScan(t, driver, testStringsToKey(""), testStringsToKey(""), store.Unlimited, nil)

	testScan(t, driver, nil, testStringsToKey("c"), store.Unlimited, all[:5])
	testScan(t, driver, []byte(""), testStringsToKey("c"), store.Unlimited, all[:5])
	testScan(t, driver, []byte("b"), nil, store.Unlimited, nil)
	testScan(t, driver, []byte("b"), testStringsToKey(""), store.Unlimited, nil)

	// testing scan with limit
	testScan(t, driver, []byte("a"), []byte("a"), 100, nil)
	testScan(t, driver, []byte("a"), []byte("b"), 1, all[:1])
	testScan(t, driver, []byte("b"), []byte("a"), 10, nil)
	testScan(t, driver, []byte("b"), []byte("bb"), 1, all[1:2])
	testScan(t, driver, []byte("b"), []byte("bb"), 2, all[1:3])
	testScan(t, driver, []byte("b"), []byte("bb"), 3, all[1:4])
	testScan(t, driver, []byte("b"), []byte("bb"), 4, all[1:4])
	testScan(t, driver, nil, nil, 100, nil)
	testScan(t, driver, testStringsToKey(""), testStringsToKey(""), 10, nil)

	testScan(t, driver, nil, testStringsToKey("c"), 1, all[:1])
	testScan(t, driver, []byte(""), testStringsToKey("c"), 3, all[:3])
	testScan(t, driver, []byte("b"), nil, 1, nil)
	testScan(t, driver, []byte("b"), testStringsToKey(""), 1, nil)

	if deletableDriver, ok := driver.(store.Deletable); ok {
		// testing Batch Deletion function
		keys := [][]byte{}
		for _, kv := range all {
			keys = append(keys, kv.Key)
		}

		err = deletableDriver.BatchDelete(context.Background(), keys)
		require.NoError(t, err)

		// testing GET with a flush
		for _, kv := range all {
			// testing GET function
			_, err := driver.Get(context.Background(), kv.Key)
			require.Error(t, err)
			assert.Equal(t, err, store.ErrNotFound)
		}
	}
}

func testPrefix(t *testing.T, driver store.KVStore, prefix []byte, limit int, exp []store.KV) {
	var got []store.KV
	itr := driver.Prefix(context.Background(), prefix, limit)
	for itr.Next() {
		got = append(got, itr.Item())
	}
	testPrintKVs(fmt.Sprintf("test prefix with prefix %q", string(prefix)), got)
	require.NoError(t, itr.Err())
	require.Equal(t, exp, got)
}

func testBatchPrefix(t *testing.T, driver store.KVStore, prefixes [][]byte, limit int, exp ...store.KV) {
	var got []store.KV
	itr := driver.BatchPrefix(context.Background(), prefixes, limit)
	for itr.Next() {
		got = append(got, itr.Item())
	}

	stringPrefixes := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		stringPrefixes[i] = string(prefix)
	}

	testPrintKVs(fmt.Sprintf("test prefixes with prefix %q", strings.Join(stringPrefixes, ", ")), got)
	require.NoError(t, itr.Err())
	require.Equal(t, exp, got)
}

func testScan(t *testing.T, driver store.KVStore, start, end []byte, limit int, exp []store.KV) {
	var got []store.KV
	itr := driver.Scan(context.Background(), start, end, limit)
	for itr.Next() {
		got = append(got, itr.Item())
	}

	testPrintKVs(fmt.Sprintf("test scan with start %q and end %q", string(start), string(end)), got)
	require.NoError(t, itr.Err())
	require.Equal(t, exp, got)
}

func testStringsToKey(parts ...string) (out []byte) {
	for _, s := range parts {
		out = append(out, []byte(s)...)
	}
	return out
}

func testPrintKVs(title string, out []store.KV) {
	if debug {
		fmt.Printf("%s\n", title)
		for _, kv := range out {
			fmt.Printf("- %s => %s\n", string(kv.Key), string(kv.Value))
		}
	}
}

func testDeleteKeyGenerate(t *testing.T, deletionTablePrefix []byte, height uint64, key []byte) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, height)
	deletionKey := append(deletionTablePrefix, buf...)
	return append(deletionKey, key...)
}
