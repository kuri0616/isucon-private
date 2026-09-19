// posts.imgdata を public/image/<id>.<ext> に一括で書き出す一回限りのツール。
// 接続情報は app 本体と同じ ISUCONP_DB_* 環境変数から読む。
// 何度実行しても既存ファイルはスキップするので冪等。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

type post struct {
	ID      int    `db:"id"`
	Mime    string `db:"mime"`
	Imgdata []byte `db:"imgdata"`
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

// app.go の saveImageFile と同じ「一時ファイル + chmod + rename」
func saveImageFile(dir string, id int, mime string, data []byte) error {
	ext := imageExt(mime)
	if ext == "" {
		return fmt.Errorf("unsupported mime: %s", mime)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), filepath.Join(dir, strconv.Itoa(id)+ext))
}

func dsn() string {
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = os.Getenv("ISUCONP_DB_PASSWORD")
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%s", host, port)
	cfg.DBName = dbname
	cfg.Params = map[string]string{"charset": "utf8mb4"}
	cfg.ParseTime = true
	cfg.Loc = time.Local

	return cfg.FormatDSN()
}

func main() {
	dir := flag.String("dir", "../public/image", "画像の出力先ディレクトリ")
	batch := flag.Int("batch", 100, "1回のSELECTで取得する件数")
	force := flag.Bool("force", false, "既存ファイルがあっても上書きする")
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatalf("failed to create %s: %s", *dir, err)
	}

	db, err := sqlx.Connect("mysql", dsn())
	if err != nil {
		log.Fatalf("failed to connect to DB: %s", err)
	}
	defer db.Close()

	// mediumblob を全件まとめてメモリに載せないよう id でページングする
	var lastID, written, skipped, failed int
	for {
		rows := []post{}
		err := db.Select(&rows,
			"SELECT `id`, `mime`, `imgdata` FROM `posts` WHERE `id` > ? ORDER BY `id` LIMIT ?",
			lastID, *batch)
		if err != nil {
			log.Fatalf("select failed after id %d: %s", lastID, err)
		}
		if len(rows) == 0 {
			break
		}

		for _, p := range rows {
			lastID = p.ID

			ext := imageExt(p.Mime)
			if ext == "" {
				log.Printf("skip post %d: unsupported mime %q", p.ID, p.Mime)
				failed++
				continue
			}

			if !*force {
				if _, err := os.Stat(filepath.Join(*dir, strconv.Itoa(p.ID)+ext)); err == nil {
					skipped++
					continue
				}
			}

			if err := saveImageFile(*dir, p.ID, p.Mime, p.Imgdata); err != nil {
				log.Printf("failed to write post %d: %s", p.ID, err)
				failed++
				continue
			}
			written++
		}

		fmt.Fprintf(os.Stderr, "\rid <= %d  written=%d skipped=%d failed=%d", lastID, written, skipped, failed)
	}

	fmt.Fprintf(os.Stderr, "\ndone: written=%d skipped=%d failed=%d (last id %d)\n", written, skipped, failed, lastID)
	if failed > 0 {
		os.Exit(1)
	}
}
