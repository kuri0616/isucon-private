package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	"github.com/go-chi/chi/v5"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
)

var (
	db         *sqlx.DB
	store      *gsm.MemcacheStore
	publicDir  string
	imageDir   string
	imageDirOK bool
)

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int
	Comments     []Comment
	User         User
	CSRFToken    string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

var memcacheClient *memcache.Client

func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient = memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

// 静的配信ディレクトリを解決する。FileServer もこの publicDir を使うので、
// 「書き出す先」と「配信する先」が食い違うことはない
func initImageDir() {
	publicDir = os.Getenv("ISUCONP_PUBLIC_DIR")
	if publicDir == "" {
		publicDir = "../public"
	}
	imageDir = filepath.Join(publicDir, "image")

	// 書けない環境 (docker の bind mount など) でも起動は止めない。
	// imageDirOK=false なら従来どおり DB から配信するだけ
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		log.Printf("image dir unavailable (%s): %s; serving images from DB only", imageDir, err)
		return
	}
	// MkdirAll は umask の影響を受けるので nginx (www-data) 向けに念のため直す
	os.Chmod(imageDir, 0o755)

	probe, err := os.CreateTemp(imageDir, ".probe-")
	if err != nil {
		log.Printf("image dir not writable (%s): %s; serving images from DB only", imageDir, err)
		return
	}
	probe.Close()
	os.Remove(probe.Name())

	imageDirOK = true
}

func dbInitialize(ctx context.Context) {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}

	for _, sql := range sqls {
		db.ExecContext(ctx, sql)
	}

	pruneImageFiles(10000)
}

func tryLogin(ctx context.Context, accountName, password string) *User {
	u := User{}
	err := db.GetContext(ctx, &u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return nil
	}

	if calculatePasshash(ctx, u.AccountName, password) == u.Passhash {
		return &u
	} else {
		return nil
	}
}

func validateUser(accountName, password string) bool {
	return regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`).MatchString(accountName) &&
		regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`).MatchString(password)
}

// 今回のGo実装では言語側のエスケープの仕組みが使えないのでOSコマンドインジェクション対策できない
// 取り急ぎPHPのescapeshellarg関数を参考に自前で実装
// cf: http://jp2.php.net/manual/ja/function.escapeshellarg.php
func escapeshellarg(arg string) string {
	return "'" + strings.Replace(arg, "'", "'\\''", -1) + "'"
}

func digest(ctx context.Context, src string) string {
	// opensslのバージョンによっては (stdin)= というのがつくので取る
	out, err := exec.CommandContext(ctx, "/bin/bash", "-c", `printf "%s" `+escapeshellarg(src)+` | openssl dgst -sha512 | sed 's/^.*= //'`).Output()
	if err != nil {
		log.Print(err)
		return ""
	}

	return strings.TrimSuffix(string(out), "\n")
}

func calculateSalt(ctx context.Context, accountName string) string {
	return digest(ctx, accountName)
}

func calculatePasshash(ctx context.Context, accountName, password string) string {
	return digest(ctx, password+":"+calculateSalt(ctx, accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")

	return session
}

func getSessionUser(r *http.Request) User {
	ctx := r.Context()
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}

	u := User{}

	err := db.GetContext(ctx, &u, "SELECT * FROM `users` WHERE `id` = ?", uid)
	if err != nil {
		return User{}
	}

	return u
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

func makePosts(ctx context.Context, results []Post, csrfToken string, allComments bool) ([]Post, error) {
	// 投稿ごとに COUNT + コメント取得を投げていた N+1 を 1 クエリにまとめる
	postIDs := make([]int, len(results))
	for i, p := range results {
		postIDs[i] = p.ID
	}

	commentsByPost, err := getCommentsByPostIDs(ctx, postIDs)
	if err != nil {
		return nil, err
	}

	limit := 3
	if allComments {
		limit = 0
	}

	// 1st pass: コメントを割り当てつつ、必要な user_id を集める
	userIDs := make(map[int]struct{}, len(results))

	for i := range results {
		p := &results[i]
		all := commentsByPost[p.ID]

		// 全件取得しているので COUNT クエリは不要
		p.CommentCount = len(all)
		p.Comments = pickComments(all, limit)

		userIDs[p.UserID] = struct{}{}
		for _, c := range p.Comments {
			userIDs[c.UserID] = struct{}{}
		}
	}

	// 投稿者とコメント者をまとめて 1 クエリで取得する (N+1 解消)
	users, err := getUsersByIDs(ctx, userIDs)
	if err != nil {
		return nil, err
	}

	// 2nd pass: 取得済みの users を割り当てる
	var posts []Post

	for i := range results {
		p := results[i]

		p.User = users[p.UserID]
		if p.User.DelFlg != 0 {
			continue
		}

		for j := range p.Comments {
			p.Comments[j].User = users[p.Comments[j].UserID]
		}

		p.CSRFToken = csrfToken

		posts = append(posts, p)
	}

	return posts, nil
}

func getUsersByIDs(ctx context.Context, idSet map[int]struct{}) (map[int]User, error) {
	users := make(map[int]User, len(idSet))
	if len(idSet) == 0 {
		return users, nil
	}

	ids := make([]int, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}

	query, args, err := sqlx.In("SELECT * FROM `users` WHERE `id` IN (?)", ids)
	if err != nil {
		return nil, err
	}

	rows := []User{}
	if err := db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, err
	}

	for _, u := range rows {
		users[u.ID] = u
	}

	return users, nil
}

// 複数投稿のコメントをまとめて 1 クエリで取得する (N+1 解消)。
// 各投稿のコメントは created_at の降順で並ぶ
func getCommentsByPostIDs(ctx context.Context, postIDs []int) (map[int][]Comment, error) {
	byPost := make(map[int][]Comment, len(postIDs))
	if len(postIDs) == 0 {
		return byPost, nil
	}

	query, args, err := sqlx.In(
		"SELECT * FROM `comments` WHERE `post_id` IN (?) ORDER BY `post_id`, `created_at` DESC",
		postIDs)
	if err != nil {
		return nil, err
	}

	rows := []Comment{}
	if err := db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, err
	}

	for _, c := range rows {
		byPost[c.PostID] = append(byPost[c.PostID], c)
	}

	return byPost, nil
}

// 取得済みコメント (created_at の降順) から表示用の並び (昇順) を作る。
// limit <= 0 なら全件。元のスライスは壊さない
func pickComments(all []Comment, limit int) []Comment {
	n := len(all)
	if limit > 0 && n > limit {
		n = limit
	}

	display := make([]Comment, n)
	for i := 0; i < n; i++ {
		display[i] = all[n-1-i]
	}

	return display
}

func imageURL(p Post) string {
	return "/image/" + strconv.Itoa(p.ID) + imageExt(p.Mime)
}

func imageExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	}
	return ""
}

func mimeForExt(ext string) string {
	switch ext {
	case "jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	}
	return ""
}

func imagePath(id int, mime string) string {
	ext := imageExt(mime)
	if ext == "" {
		return ""
	}
	return filepath.Join(imageDir, strconv.Itoa(id)+ext)
}

// 画像は nginx が /image/ を静的配信するのでファイルとして置く。
// 同一ディレクトリの一時ファイル + rename でアトミックに差し替える
// (直接書くと nginx が書きかけのファイルを配信してしまう)
func saveImageFile(id int, mime string, data []byte) error {
	if !imageDirOK {
		return nil
	}

	p := imagePath(id, mime)
	if p == "" {
		return fmt.Errorf("unsupported mime: %s", mime)
	}

	tmp, err := os.CreateTemp(imageDir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // rename が成功していれば no-op

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp は 0600 で作るので nginx (www-data) から読めない
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), p)
}

// DELETE FROM posts WHERE id > 10000 に合わせてファイル側も揃える。
// 残しておくと AUTO_INCREMENT が巻き戻ったときに前回の画像を配信してしまう
func pruneImageFiles(maxID int) {
	if !imageDirOK {
		return
	}

	entries, err := os.ReadDir(imageDir)
	if err != nil {
		log.Print(err)
		return
	}

	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".tmp-") || strings.HasPrefix(name, ".probe-") {
			os.Remove(filepath.Join(imageDir, name))
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		if id, err := strconv.Atoi(base); err == nil && id > maxID {
			os.Remove(filepath.Join(imageDir, name))
		}
	}
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

func getInitialize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dbInitialize(ctx)
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("login.html")),
	).Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(ctx, r.FormValue("account_name"), r.FormValue("password"))

	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "アカウント名かパスワードが間違っています"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("register.html")),
	).Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "アカウント名は3文字以上、パスワードは6文字以上である必要があります"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ユーザーが存在しない場合はエラーになるのでエラーチェックはしない
	db.GetContext(ctx, &exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "アカウント名がすでに使われています"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.ExecContext(ctx, query, accountName, calculatePasshash(ctx, accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	session.Values["user_id"] = uid
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)

	results := []Post{}

	err := db.SelectContext(ctx, &results, "SELECT `p`.`id`, `p`.`user_id`, `p`.`body`, `p`.`mime`, `p`.`created_at` FROM `posts` AS `p` JOIN `users` AS `u` ON `u`.`id` = `p`.`user_id` WHERE `u`.`del_flg` = 0 ORDER BY `p`.`created_at` DESC LIMIT ?", postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("index.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	)).Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
}

func getAccountName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountName := r.PathValue("accountName")
	user := User{}

	err := db.GetContext(ctx, &user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", accountName)
	if err != nil {
		log.Print(err)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}

	err = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC LIMIT ?", user.ID, postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	commentCount := 0
	err = db.GetContext(ctx, &commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	// 以前は id を全件引いてから手組みの IN (...) に渡していたが、
	// 件数しか使わないので集約クエリ 2 本にする
	postCount := 0
	err = db.GetContext(ctx, &postCount, "SELECT COUNT(*) AS count FROM `posts` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	commentedCount := 0
	err = db.GetContext(ctx, &commentedCount,
		"SELECT COUNT(*) AS count FROM `comments` AS c JOIN `posts` AS p ON p.id = c.post_id WHERE p.user_id = ?",
		user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	me := getSessionUser(r)

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("user.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	)).Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

func getPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}

	results := []Post{}
	err = db.SelectContext(ctx, &results, "SELECT `p`.`id`, `p`.`user_id`, `p`.`body`, `p`.`mime`, `p`.`created_at` FROM `posts` AS `p` JOIN `users` AS `u` ON `u`.`id` = `p`.`user_id` WHERE `p`.`created_at` <= ? AND `u`.`del_flg` = 0 ORDER BY `p`.`created_at` DESC LIMIT ?", t.Format(ISO8601Format), postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	)).Execute(w, posts)
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	// imgdata は使わないので読まない (mediumblob の転送を避ける)
	err = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), true)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := posts[0]

	me := getSessionUser(r)

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("post_id.html"),
		getTemplPath("post.html"),
	)).Execute(w, struct {
		Post Post
		Me   User
	}{p, me})
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "画像が必須です"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	if file != nil {
		// 投稿のContent-Typeからファイルのタイプを決定する
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "投稿できる画像形式はjpgとpngとgifだけです"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}

	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "ファイルサイズが大きすぎます"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`) VALUES (?,?,?,?)"
	result, err := db.ExecContext(
		ctx,
		query,
		me.ID,
		mime,
		filedata,
		r.FormValue("body"),
	)
	if err != nil {
		log.Print(err)
		return
	}

	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}

	if err := saveImageFile(int(pid), mime, filedata); err != nil {
		// 画像は DB にも入っているので、失敗しても getImage の lazy 生成で回復できる。
		// 投稿自体は成功させる
		log.Print(err)
	}

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

func getImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	ext := r.PathValue("ext")
	wantMime := mimeForExt(ext)
	if wantMime == "" {
		// 対応外の拡張子はどの投稿とも一致しえないので DB を見るまでもない
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// 通常は nginx が try_files で静的配信するのでここには来ない。
	// nginx を経由しない直アクセス用に、ファイルがあれば DB を叩かず返す
	if imageDirOK {
		if f, err := os.Open(imagePath(pid, wantMime)); err == nil {
			defer f.Close()
			// 中身が空なら壊れているので DB から読み直す (lazy 生成でファイルが治る)
			if st, err := f.Stat(); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
				w.Header().Set("Content-Type", wantMime)
				w.Header().Set("Cache-Control", "public, max-age=86400")
				http.ServeContent(w, r, "", st.ModTime(), f)
				return
			}
		}
	}

	// ファイルが無い分だけ DB から読み出し、ついでに書き出しておく (lazy 生成)
	post := Post{}
	err = db.GetContext(ctx, &post, "SELECT `mime`, `imgdata` FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if post.Mime != wantMime {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if err := saveImageFile(pid, post.Mime, post.Imgdata); err != nil {
		log.Print(err) // 書き出しに失敗しても配信は続ける
	}

	w.Header().Set("Content-Type", post.Mime)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if _, err := w.Write(post.Imgdata); err != nil {
		log.Print(err)
	}
}

func postComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_idは整数のみです")
		return
	}

	query := "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)"
	_, err = db.ExecContext(ctx, query, postID, me.ID, r.FormValue("comment"))
	if err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.SelectContext(ctx, &users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		log.Print(err)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("banned.html")),
	).Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	err := r.ParseForm()
	if err != nil {
		log.Print(err)
		return
	}

	for _, id := range r.Form["uid[]"] {
		db.ExecContext(ctx, query, 1, id)
	}

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

func main() {
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%s", host, port)
	cfg.DBName = dbname
	cfg.Params = map[string]string{
		"charset": "utf8mb4",
		// プレースホルダをクライアント側で展開し、
		// Prepare/Execute/Close の3往復を COM_QUERY 1往復に削減する
		"interpolateParams": "true",
	}
	cfg.ParseTime = true
	cfg.Loc = time.Local
	dsn := cfg.FormatDSN()

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()

	initImageDir()

	r := chi.NewRouter()

	r.Get("/initialize", getInitialize)
	r.Get("/login", getLogin)
	r.Post("/login", postLogin)
	r.Get("/register", getRegister)
	r.Post("/register", postRegister)
	r.Get("/logout", getLogout)
	r.Get("/", getIndex)
	r.Get("/posts", getPosts)
	r.Get("/posts/{id}", getPostsID)
	r.Post("/", postIndex)
	r.Get("/image/{id}.{ext}", getImage)
	// public/image/ が実在するようになったので、これが無いと下の FileServer が
	// 1万件のディレクトリ一覧を返してしまう
	r.Get("/image/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	r.Post("/comment", postComment)
	r.Get("/admin/banned", getAdminBanned)
	r.Post("/admin/banned", postAdminBanned)
	r.Get(`/@{accountName:[0-9a-zA-Z_]+}`, getAccountName)
	r.Mount("/", http.FileServer(http.Dir(publicDir)))

	log.Fatal(http.ListenAndServe(":8080", r))
}
