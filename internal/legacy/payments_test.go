package legacy

import (
	"reflect"
	"testing"
)

func TestPHPArray(t *testing.T) {
	got := phpArray(`a:3:{i:0;s:4:"1066";i:2;i:5;s:3:"key";s:15:"fine_category-7";}`)
	want := map[string]string{"0": "1066", "2": "5", "key": "fine_category-7"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("phpArray = %v", got)
	}
	if k := phpKeys(`a:2:{i:3;s:1:"a";i:1;s:1:"b";}`); !reflect.DeepEqual(k, []string{"3", "1"}) {
		t.Fatalf("phpKeys = %v", k)
	}
	for _, s := range []string{"N;", "", "a:0:{}", "garbage", `a:1:{i:0;s:9:"short";}`} {
		if m := phpArray(s); len(m) != 0 {
			t.Errorf("phpArray(%q) = %v", s, m)
		}
	}
}
