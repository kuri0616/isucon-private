package main

import (
	"testing"
	"time"
)

// created_at 降順のコメント列を作る (id が大きいほど新しい)
func descComments(ids ...int) []Comment {
	base := time.Date(2016, 1, 2, 11, 0, 0, 0, time.Local)
	cs := make([]Comment, len(ids))
	for i, id := range ids {
		cs[i] = Comment{
			ID:        id,
			PostID:    1,
			UserID:    id * 10,
			Comment:   "c",
			CreatedAt: base.Add(time.Duration(-i) * time.Minute),
		}
	}
	return cs
}

func idsOf(cs []Comment) []int {
	out := make([]int, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPickCommentsLimitsToNewestAndReverses(t *testing.T) {
	// 降順で 5,4,3,2,1 (5 が最新)
	all := descComments(5, 4, 3, 2, 1)

	got := pickComments(all, 3)

	// 最新3件 (5,4,3) を古い順に並べ替えた 3,4,5 になること
	if want := []int{3, 4, 5}; !equalInts(idsOf(got), want) {
		t.Fatalf("got %v, want %v", idsOf(got), want)
	}

	// 元のスライスが壊れていないこと (in-place 反転だと 3,4,5,2,1 になる)
	if want := []int{5, 4, 3, 2, 1}; !equalInts(idsOf(all), want) {
		t.Fatalf("source mutated: %v, want %v", idsOf(all), want)
	}
}

func TestPickCommentsAllCommentsReturnsEverything(t *testing.T) {
	all := descComments(4, 3, 2, 1)

	// allComments = true のとき limit=0 で呼ばれる
	got := pickComments(all, 0)

	if want := []int{1, 2, 3, 4}; !equalInts(idsOf(got), want) {
		t.Fatalf("got %v, want %v", idsOf(got), want)
	}
}

func TestPickCommentsFewerThanLimit(t *testing.T) {
	all := descComments(2, 1)

	got := pickComments(all, 3)

	if want := []int{1, 2}; !equalInts(idsOf(got), want) {
		t.Fatalf("got %v, want %v", idsOf(got), want)
	}
}

func TestPickCommentsEmptyIsNonNil(t *testing.T) {
	// コメントが 0 件の投稿では map に entry が無く nil が渡る。
	// テンプレートの {{range}} 用に長さ0のスライスを返すこと
	for _, in := range [][]Comment{nil, {}} {
		got := pickComments(in, 3)
		if got == nil {
			t.Fatal("returned nil slice, want empty slice")
		}
		if len(got) != 0 {
			t.Fatalf("len = %d, want 0", len(got))
		}
	}
}

// CommentCount は表示件数ではなく総件数であること。
// makePosts は len(all) を使い、pickComments の結果は使わない
func TestCommentCountIsTotalNotDisplayed(t *testing.T) {
	all := descComments(9, 8, 7, 6, 5, 4, 3, 2, 1)

	displayed := pickComments(all, 3)

	if len(all) != 9 {
		t.Fatalf("total = %d, want 9", len(all))
	}
	if len(displayed) != 3 {
		t.Fatalf("displayed = %d, want 3", len(displayed))
	}
}
