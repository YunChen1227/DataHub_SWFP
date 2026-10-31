package application

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/port"
	"github.com/datahub/relay/internal/domain/zerorule"
)

// rawSection 对应寻源分段 JSON 里的一段（与 upstream.aggSection 同形）。
type rawSection struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

// payload 组装本次请求的原始数据留存。每个请求都有一行（含鉴权/参数失败，入参
// 同样是对账证据）；上游源行只写查得或带回了数据的源。
func (t bookTask) payload() *model.RequestPayload {
	if t.rec == nil || t.rec.RequestID == "" {
		return nil
	}
	at := t.rec.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	p := &model.RequestPayload{
		RequestID:  t.rec.RequestID,
		AppKey:     t.rec.AppKey,
		Version:    t.rec.Version,
		Reqid:      t.rec.Reqid,
		CreditCode: t.rec.CreditCode,
		CreatedAt:  at,
	}
	p.RuleVersion = zerorule.Version
	if t.cmd != nil {
		p.RequestBody, _ = json.Marshal(t.cmd)
	}
	var res *model.UpstreamResult
	if t.decision != nil {
		res = t.decision.Result
	}
	if res == nil {
		return p
	}
	p.ResponseRange = asJSON(res.Range)

	var sections map[string]rawSection
	if res.RawRange != "" {
		_ = json.Unmarshal([]byte(res.RawRange), &sections)
	}
	var merged []zerorule.Source
	for _, c := range t.sources {
		sec, ok := sections[c.Label]
		if !ok || c.Status == model.CallSkipped {
			continue
		}
		if c.Status != model.CallOK && len(sec.Data) == 0 {
			continue
		}
		amounts := zerorule.Extract(c.Label, sec.Data, at)
		var got model.DimSet
		if c.Status == model.CallOK {
			got = c.Dims
			merged = append(merged, zerorule.Source{Amounts: amounts, Got: got})
		}
		u := &model.UpstreamPayload{
			RequestID:   p.RequestID,
			AppKey:      p.AppKey,
			Version:     p.Version,
			Reqid:       p.Reqid,
			SourceLabel: c.Label,
			SourceName:  c.Source,
			Provider:    c.Provider,
			Status:      c.Status,
			RawData:     asJSON(string(sec.Data)),
			ZeroVerdict: toVerdict(zerorule.Judge(amounts, got)),
			CreatedAt:   at,
		}
		p.Sources = append(p.Sources, u)
	}
	p.ZeroVerdict = toVerdict(zerorule.Merge(merged))
	return p
}

func toVerdict(v zerorule.Verdict) model.ZeroVerdict {
	return model.ZeroVerdict{
		InvAmt12m:   money(v.InvAmt),
		TaxAmt12m:   money(v.TaxAmt),
		InvZero:     v.InvZero,
		TaxZero:     v.TaxZero,
		RuleVersion: zerorule.Version,
	}
}

func money(r *big.Rat) *string {
	if r == nil {
		return nil
	}
	s := r.FloatString(2)
	return &s
}

// asJSON 把一段文本转成可入 jsonb 的 JSON：本身合法则原样，否则存为 JSON 字符串，
// 绝不因格式问题丢数据。
func asJSON(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

// PayloadSpool 暂存写库失败的原始数据：每条一行 JSON，按天分文件
// (YYYYMMDD.jsonl)。Replay 把文件改名为 .replay 后逐条重放，失败的条目写回
// spool，成功即删文件；进程在重放中途退出时，残留的 .replay 文件下次照样重放。
// 写入是幂等的，重复重放无害。
type PayloadSpool struct {
	dir string
	log *slog.Logger
	mu  sync.Mutex
}

func NewPayloadSpool(dir string, log *slog.Logger) *PayloadSpool {
	if log == nil {
		log = slog.Default()
	}
	return &PayloadSpool{dir: dir, log: log}
}

// Put 追加一条待重放记录。
func (s *PayloadSpool) Put(p *model.RequestPayload) error {
	line, err := json.Marshal(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	name := filepath.Join(s.dir, time.Now().Format("20060102")+".jsonl")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Replay 把 spool 里的记录写回仓储，返回成功条数。
func (s *PayloadSpool) Replay(ctx context.Context, repo port.PayloadRepository) (int, error) {
	files, err := s.claim()
	if err != nil {
		return 0, err
	}
	done := 0
	for _, name := range files {
		n, err := s.replayFile(ctx, repo, name)
		done += n
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

// claim 把当前的 .jsonl 改名为 .replay（与 Put 互斥），连同残留的 .replay 一起返回。
func (s *PayloadSpool) claim() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := filepath.Join(s.dir, e.Name())
		switch {
		case e.IsDir():
		case strings.HasSuffix(name, ".jsonl"):
			claimed := strings.TrimSuffix(name, ".jsonl") + fmt.Sprintf(".%d.replay", time.Now().UnixNano())
			if err := os.Rename(name, claimed); err != nil {
				return nil, err
			}
			out = append(out, claimed)
		case strings.HasSuffix(name, ".replay"):
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *PayloadSpool) replayFile(ctx context.Context, repo port.PayloadRepository, name string) (int, error) {
	f, err := os.Open(name)
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	done, failed := 0, 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var p model.RequestPayload
		if err := json.Unmarshal(line, &p); err != nil {
			s.log.Error("payload spool: unparsable line moved to .bad", "file", name, "err", err)
			if berr := s.keepBad(line); berr != nil {
				f.Close()
				return done, berr
			}
			continue
		}
		if err := repo.AppendPayload(ctx, &p); err != nil {
			failed++
			if perr := s.Put(&p); perr != nil {
				f.Close()
				return done, fmt.Errorf("payload spool: requeue %s: %w", p.RequestID, perr)
			}
			continue
		}
		done++
	}
	scanErr := sc.Err()
	f.Close()
	if scanErr != nil {
		return done, fmt.Errorf("payload spool: read %s: %w", name, scanErr)
	}
	if failed > 0 {
		s.log.Warn("payload spool: some records still failing, requeued", "file", name, "failed", failed)
	}
	return done, os.Remove(name)
}

// keepBad 保留无法解析的行（多为崩溃时写了一半），供人工处理，不参与重放。
func (s *PayloadSpool) keepBad(line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.dir, "unparsable.bad"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(append([]byte(nil), line...), '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Run 启动时立即重放一次，之后每 interval 重放一次，直至 ctx 取消。
func (s *PayloadSpool) Run(ctx context.Context, repo port.PayloadRepository, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, interval)
		n, err := s.Replay(rctx, repo)
		cancel()
		if err != nil {
			s.log.Error("payload spool replay failed", "replayed", n, "err", err)
		} else if n > 0 {
			s.log.Info("payload spool replayed", "replayed", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
