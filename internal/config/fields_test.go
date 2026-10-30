package config

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fields.txt 是 ferry.yaml 的全部字段路径（映射的任意键写 *，列表元素写 []）。
// 桌面版的结构说明（desktop/src/editor/schema.ts）也对照这个文件检查：加了字段两边都要改。
// 字段有变化时用 FERRY_UPDATE_FIELDS=1 go test ./internal/config -run TestFieldList 重新生成。
func TestFieldList(t *testing.T) {
	var paths []string
	walkFields(reflect.TypeOf(File{}), "", &paths)
	sort.Strings(paths)
	got := strings.Join(paths, "\n") + "\n"

	const file = "testdata/fields.txt"
	if os.Getenv("FERRY_UPDATE_FIELDS") == "1" {
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Fatalf("配置字段和 %s 不一致（加了字段要同时改 desktop/src/editor/schema.ts）；用 FERRY_UPDATE_FIELDS=1 重新生成。现在的字段：\n%s", file, got)
	}
}

var (
	buildRefType   = reflect.TypeOf(BuildRef{})
	durationType   = reflect.TypeOf(Duration(0))
	stringListType = reflect.TypeOf(StringList{})
)

func walkFields(t reflect.Type, prefix string, out *[]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		p := name
		if prefix != "" {
			p = prefix + "." + name
		}
		*out = append(*out, p)
		walkType(f.Type, p, out)
	}
}

func walkType(t reflect.Type, p string, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == durationType || t == stringListType:
	case t == buildRefType:
		walkFields(reflect.TypeOf(Build{}), p, out)
	case t.Kind() == reflect.Struct:
		walkFields(t, p, out)
	case t.Kind() == reflect.Map && elemStruct(t.Elem()):
		walkType(t.Elem(), p+".*", out)
	case t.Kind() == reflect.Slice && elemStruct(t.Elem()):
		walkType(t.Elem(), p+".[]", out)
	}
}

func elemStruct(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct && t != durationType
}
