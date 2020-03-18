package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v4"
	"github.com/joshuarubin/go-sway"
	"gopkg.in/tomb.v2"
)

type Time struct {
	time.Time
}

func (t Time) MarshalJSON() (bs []byte, err error) {
	return []byte(`"` + t.UTC().Format(time.RFC3339Nano) + `"`), nil
}

type Target struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

func NewTarget(node *sway.Node) (target *Target) {
	if node == nil {
		return nil
	}

	if node.Type != "con" {
		return nil
	}

	target = &Target{}

	if node.WindowProperties != nil {
		target.Name = node.WindowProperties.Instance
	}

	if node.AppID != nil {
		target.Name = *node.AppID
	}

	target.Detail = node.Name

	return target
}

func (t *Target) Equal(o *Target) bool {
	if o == nil {
		return false
	}

	if t.Name != o.Name {
		return false
	}

	if t.Detail != o.Detail {
		return false
	}

	return true
}

type FocusRecord struct {
	Start    Time    `json:"start"`
	Stop     Time    `json:"stop"`
	Duration float64 `json:"duration"`

	Target
}

type FocusTracker struct {
	p Persistence

	Start  Time    `json:"start"`
	Target *Target `json:"target"`
}

func NewFocusTracker(p Persistence) (ft *FocusTracker, err error) {
	return &FocusTracker{
		p: p,
	}, nil
}

func (f *FocusTracker) Update(ctx context.Context, node *sway.Node) (err error) {
	if f.Target == nil && node == nil {
		return
	}

	now := time.Now()
	target := NewTarget(node)

	if f.Target != nil {
		if f.Target.Equal(target) {
			return
		}

		d := now.Sub(f.Start.Time)

		err = f.p.Save(ctx, &FocusRecord{
			Target:   *f.Target,
			Start:    f.Start,
			Stop:     Time{now},
			Duration: d.Seconds(),
		})
		if err != nil {
			return
		}

		fmt.Fprintf(os.Stderr, "Duration %s\n", d)
	}

	f.Target = target
	f.Start = Time{now}

	if f.Target != nil {
		fmt.Fprintf(os.Stderr, "Focusing on Name %q Detail %q\n", f.Target.Name, f.Target.Detail)
	} else {
		fmt.Fprintf(os.Stderr, "Focusing on nothing...\n")
	}

	return
}

type Persistence interface {
	Save(ctx context.Context, fr *FocusRecord) error
	Close(ctx context.Context) error
}

type JsonFilePersistence struct {
	x sync.Mutex
	f *os.File
}

var _ Persistence = (*JsonFilePersistence)(nil)

func NewJsonFilePersistence(f *os.File) (jfp *JsonFilePersistence) {
	return &JsonFilePersistence{
		f: f,
	}
}

func (jfp *JsonFilePersistence) Save(ctx context.Context, fr *FocusRecord) (err error) {
	jfp.x.Lock()
	defer jfp.x.Unlock()

	b, err := json.Marshal(fr)
	if err != nil {
		return
	}

	_, err = jfp.f.WriteString(string(b) + "\n")
	if err != nil {
		return
	}
	jfp.f.Sync()

	return
}

func (jfp *JsonFilePersistence) Close(ctx context.Context) (err error) {
	jfp.x.Lock()
	defer jfp.x.Unlock()

	return jfp.f.Close()
}

type PostgresPersistence struct {
	conn *pgx.Conn
}

var _ Persistence = (*PostgresPersistence)(nil)

func NewPostgresPersistence(ctx context.Context, url string) (pp *PostgresPersistence, err error) {
	pp = &PostgresPersistence{}

	pp.conn, err = pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}

	return
}

func (pp *PostgresPersistence) Save(ctx context.Context, fr *FocusRecord) (err error) {
	query := `INSERT INTO focus (start, stop, duration, name, detail) VALUES ($1, $2, $3, $4, $5)`

	_, err = pp.conn.Exec(ctx, query, fr.Start, fr.Stop, fr.Duration, fr.Name, fr.Detail)
	if err != nil {
		return
	}

	return
}

func (pp *PostgresPersistence) Close(ctx context.Context) (err error) {
	return pp.conn.Close(ctx)
}

type Watcher struct {
	ft *FocusTracker

	sway.EventHandler
}

func NewWatcher(ft *FocusTracker) (w *Watcher, err error) {
	return &Watcher{
		ft: ft,
	}, nil
}

func (w Watcher) Workspace(ctx context.Context, e sway.WorkspaceEvent) {
	if e.Change != "focus" {
		return
	}

	if e.Current == nil {
		return
	}

	w.ft.Update(ctx, e.Current.FocusedNode())
}

var windowFilter = map[string]bool{
	"focus": true,
	"title": true,
}

func (w Watcher) Window(ctx context.Context, e sway.WindowEvent) {
	if _, ok := windowFilter[e.Change]; ok {
		w.ft.Update(ctx, &e.Container)
	}
}

func main() {
	var err error

	t, ctx := tomb.WithContext(context.Background())

	p := NewJsonFilePersistence(os.Stdout)

	ft, err := NewFocusTracker(p)
	if err != nil {
		panic(err)
	}

	// Register signal handling for graceful exit.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	t.Go(func() (err error) {
		s := <-sigs

		fmt.Fprintf(os.Stderr, "User requested exit %s\n", s)

		// This is necessary to force a save of the current focus to
		// the persistence layer.
		err = ft.Update(ctx, nil)
		if err != nil {
			return
		}

		t.Kill(nil)

		return
	})

	// Set our initial focus.
	client, err := sway.New(ctx)
	if err != nil {
		panic(err)
	}

	nodes, err := client.GetTree(ctx)
	if err != nil {
		panic(err)
	}

	err = ft.Update(ctx, nodes.FocusedNode())
	if err != nil {
		panic(err)
	}

	// Occassionally poll to detect if nothing is focused. This happens if
	// the screen lock is activated since no event is generated in that
	// case.
	t.Go(func() (err error) {
		for {
			select {
			case <-time.After(10 * time.Second):
				nodes, err := client.GetTree(ctx)
				if err != nil {
					return err
				}

				focused := nodes.FocusedNode()
				if focused == nil {
					err = ft.Update(ctx, nil)
					if err != nil {
						return err
					}
				}
			case <-t.Dying():
				return
			}
		}
	})

	// Intialize our Sway event handler and begin watching for events.
	eh, err := NewWatcher(ft)
	if err != nil {
		panic(err)
	}

	t.Go(func() (err error) {
		return sway.Subscribe(ctx, eh,
			sway.EventTypeWorkspace,
			sway.EventTypeWindow,
		)
	})

	// Wait here for the background processes to exit.
	err = t.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		panic(err)
	}
}
