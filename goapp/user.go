/*
 * Copyright (c) 2013 Matt Jibson <matt.jibson@gmail.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package goapp

import (
	"appengine"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/gob"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"appengine/blobstore"
	"appengine/datastore"
	"appengine/taskqueue"
	"appengine/user"
	mpg "github.com/MiniProfiler/go/miniprofiler_gae"
	"github.com/mjibson/goon"
	"goapp/sanitizer"
)

func LoginGoogle(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	if cu := user.Current(c); cu != nil {
		gn := goon.FromContext(c)
		u := &User{Id: cu.ID}
		if err := gn.Get(u); err == datastore.ErrNoSuchEntity {
			u.Email = cu.Email
			u.Read = time.Now().Add(-time.Hour * 24)
			gn.Put(u)
		}
	}

	http.Redirect(w, r, routeUrl("main"), http.StatusFound)
}

func Logout(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	if appengine.IsDevAppServer() {
		if u, err := user.LogoutURL(c, routeUrl("main")); err == nil {
			http.Redirect(w, r, u, http.StatusFound)
			return
		}
	} else {
		http.SetCookie(w, &http.Cookie{
			Name:    "ACSID",
			Value:   "",
			Expires: time.Time{},
		})
	}
	http.Redirect(w, r, routeUrl("main"), http.StatusFound)
}

func ImportOpml(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	cu := user.Current(c)
	gn := goon.FromContext(c)
	u := User{Id: cu.ID}
	if err := gn.Get(&u); err != nil {
		serveError(w, err)
		return
	}
	backupOPML(c)

	if file, _, err := r.FormFile("file"); err == nil {
		if fdata, err := ioutil.ReadAll(file); err == nil {
			buf := bytes.NewReader(fdata)
			// attempt to extract from google reader takeout zip
			if zb, zerr := zip.NewReader(buf, int64(len(fdata))); zerr == nil {
				for _, f := range zb.File {
					if strings.HasSuffix(f.FileHeader.Name, "Reader/subscriptions.xml") {
						if rc, rerr := f.Open(); rerr == nil {
							if fb, ferr := ioutil.ReadAll(rc); ferr == nil {
								fdata = fb
								break
							}
						}
					}
				}
			}

			bk, err := saveFile(c, fdata)
			if err != nil {
				serveError(w, err)
				return
			}
			task := taskqueue.NewPOSTTask(routeUrl("import-opml-task"), url.Values{
				"key":  {string(bk)},
				"user": {cu.ID},
			})
			taskqueue.Add(c, task, "import-reader")
		}
	}
}

func AddSubscription(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	backupOPML(c)
	cu := user.Current(c)
	url := r.FormValue("url")
	o := &OpmlOutline{
		Outline: []*OpmlOutline{
			&OpmlOutline{XmlUrl: url},
		},
	}
	if err := addFeed(c, cu.ID, o); err != nil {
		c.Errorf("add sub error (%s): %s", url, err.Error())
		serveError(w, err)
		return
	}

	gn := goon.FromContext(c)
	ud := UserData{Id: "data", Parent: gn.Key(&User{Id: cu.ID})}
	gn.Get(&ud)
	if err := mergeUserOpml(c, &ud, o); err != nil {
		c.Errorf("add sub error opml (%v): %v", url, err)
		serveError(w, err)
		return
	}
	gn.PutMany(&ud, &Log{
		Parent: ud.Parent,
		Id:     time.Now().UnixNano(),
		Text:   fmt.Sprintf("add sub: %v", url),
	})
	if r.Method == "GET" {
		http.Redirect(w, r, routeUrl("main"), http.StatusFound)
	}
	backupOPML(c)
}

func saveFile(c appengine.Context, b []byte) (appengine.BlobKey, error) {
	w, err := blobstore.Create(c, "application/json")
	if err != nil {
		return "", err
	}
	if _, err := w.Write(b); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return w.Key()
}

const oldDuration = time.Hour * 24 * 7 * 2 // two weeks
const numStoriesLimit = 1000

func ListFeeds(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	cu := user.Current(c)
	gn := goon.FromContext(c)
	u := &User{Id: cu.ID}
	ud := &UserData{Id: "data", Parent: gn.Key(u)}
	if err := gn.GetMulti([]interface{}{u, ud}); err != nil && !goon.NotFound(err, 1) {
		serveError(w, err)
		return
	}
	l := &Log{
		Parent: ud.Parent,
		Id:     time.Now().UnixNano(),
		Text:   "list feeds",
	}
	l.Text += fmt.Sprintf(", len opml %v", len(ud.Opml))
	putU := false
	putUD := false
	fixRead := false
	if time.Since(u.Read) > oldDuration {
		u.Read = time.Now().Add(-oldDuration)
		putU = true
		fixRead = true
		l.Text += ", u.Read"
	}

	read := make(Read)
	var uf Opml
	c.Step("unmarshal user data", func(c mpg.Context) {
		gob.NewDecoder(bytes.NewReader(ud.Read)).Decode(&read)
		json.Unmarshal(ud.Opml, &uf)
	})
	var feeds []*Feed
	opmlMap := make(map[string]*OpmlOutline)
	var merr error
	c.Step("fetch feeds", func(c mpg.Context) {
		gn := goon.FromContext(c)
		for _, outline := range uf.Outline {
			if outline.XmlUrl == "" {
				for _, so := range outline.Outline {
					feeds = append(feeds, &Feed{Url: so.XmlUrl})
					opmlMap[so.XmlUrl] = so
				}
			} else {
				feeds = append(feeds, &Feed{Url: outline.XmlUrl})
				opmlMap[outline.XmlUrl] = outline
			}
		}
		merr = gn.GetMulti(feeds)
	})
	lock := sync.Mutex{}
	fl := make(map[string][]*Story)
	q := datastore.NewQuery(gn.Key(&Story{}).Kind())
	hasStories := false
	updatedLinks := false
	icons := make(map[string]string)
	noads := make(map[string]bool)
	now := time.Now()
	numStories := 0

	c.Step("feed fetch + wait", func(c mpg.Context) {
		queue := make(chan *Feed)
		tc := make(chan *taskqueue.Task)
		done := make(chan bool)
		wg := sync.WaitGroup{}
		feedProc := func() {
			for f := range queue {
				c.Step(f.Title, func(c mpg.Context) {
					defer wg.Done()
					var stories []*Story
					gn := goon.FromContext(c)

					if !f.Date.Before(u.Read) {
						fk := gn.Key(f)
						sq := q.Ancestor(fk).Filter(IDX_COL+" >=", u.Read).KeysOnly().Order("-" + IDX_COL)
						keys, _ := gn.GetAll(sq, nil)
						stories = make([]*Story, len(keys))
						for j, key := range keys {
							stories[j] = &Story{
								Id:     key.StringID(),
								Parent: fk,
							}
						}
						gn.GetMulti(stories)
					}
					if f.Link != opmlMap[f.Url].HtmlUrl {
						l.Text += fmt.Sprintf(", link: %v -> %v", opmlMap[f.Url].HtmlUrl, f.Link)
						updatedLinks = true
						opmlMap[f.Url].HtmlUrl = f.Link
					}
					manualDone := false
					if time.Since(f.LastViewed) > time.Hour*24*2 {
						if f.NextUpdate.Equal(timeMax) {
							tc <- taskqueue.NewPOSTTask(routeUrl("update-feed-manual"), url.Values{
								"feed": {f.Url},
								"last": {"1"},
							})
							manualDone = true
						} else {
							tc <- taskqueue.NewPOSTTask(routeUrl("update-feed-last"), url.Values{
								"feed": {f.Url},
							})
						}
					}
					if !manualDone && now.Sub(f.NextUpdate) >= 0 {
						tc <- taskqueue.NewPOSTTask(routeUrl("update-feed-manual"), url.Values{
							"feed": {f.Url},
						})
					}
					lock.Lock()
					fl[f.Url] = stories
					numStories += len(stories)
					if len(stories) > 0 {
						hasStories = true
					}
					if f.Image != "" {
						icons[f.Url] = f.Image
					}
					if f.NoAds {
						noads[f.Url] = true
					}
					lock.Unlock()
				})
			}
		}
		go taskSender(c, "update-manual", tc, done)
		for i := 0; i < 20; i++ {
			go feedProc()
		}
		for i, f := range feeds {
			if goon.NotFound(merr, i) {
				continue
			}
			wg.Add(1)
			queue <- f
		}
		close(queue)
		// wait for feeds to complete so there are no more tasks to queue
		wg.Wait()
		// then finish enqueuing tasks
		close(tc)
		<-done
	})
	if numStories > 0 {
		c.Step("numStories", func(c mpg.Context) {
			stories := make([]*Story, 0, numStories)
			for _, v := range fl {
				stories = append(stories, v...)
			}
			sort.Sort(sort.Reverse(Stories(stories)))
			if len(stories) > numStoriesLimit {
				stories = stories[:numStoriesLimit]
				fl = make(map[string][]*Story)
				for _, s := range stories {
					fk := s.Parent.StringID()
					p := fl[fk]
					fl[fk] = append(p, s)
				}
			}
			last := stories[len(stories)-1].Created
			if !u.Read.Equal(last) {
				u.Read = last
				putU = true
				fixRead = true
			}
		})
	}
	if fixRead {
		c.Step("fix read", func(c mpg.Context) {
			nread := make(Read)
			for k, v := range fl {
				for _, s := range v {
					rs := readStory{Feed: k, Story: s.Id}
					if read[rs] {
						nread[rs] = true
					}
				}
			}
			if len(nread) != len(read) {
				read = nread
				var b bytes.Buffer
				gob.NewEncoder(&b).Encode(&read)
				ud.Read = b.Bytes()
				putUD = true
				l.Text += ", fix read"
			}
		})
	}
	numStories = 0
	for k, v := range fl {
		newStories := make([]*Story, 0, len(v))
		for _, s := range v {
			if !read[readStory{Feed: k, Story: s.Id}] {
				newStories = append(newStories, s)
			}
		}
		numStories += len(newStories)
		fl[k] = newStories
	}
	if numStories == 0 {
		l.Text += ", clear read"
		fixRead = false
		if ud.Read != nil {
			putUD = true
			ud.Read = nil
		}
		last := u.Read
		for _, v := range feeds {
			if last.Before(v.Date) {
				last = v.Date
			}
		}
		c.Infof("nothing here, move up: %v -> %v", u.Read, last)
		if !u.Read.Equal(last) {
			putU = true
			u.Read = last
		}
	}
	if !hasStories {
		var last time.Time
		for _, f := range feeds {
			if last.Before(f.Date) {
				last = f.Date
			}
		}
		if u.Read.Before(last) {
			c.Debugf("setting %v read to %v", cu.ID, last)
			putU = true
			putUD = true
			u.Read = last
			ud.Read = nil
			l.Text += ", read last"
		}
	}
	if updatedLinks {
		backupOPML(c)
		if o, err := json.Marshal(&uf); err == nil {
			ud.Opml = o
			putUD = true
			l.Text += ", update links"
		} else {
			c.Errorf("json UL err: %v, %v", err, uf)
		}
	}
	if putU {
		gn.Put(u)
		l.Text += ", putU"
	}
	if putUD {
		gn.Put(ud)
		l.Text += ", putUD"
	}
	l.Text += fmt.Sprintf(", len opml %v", len(ud.Opml))
	gn.Put(l)
	c.Step("json marshal", func(c mpg.Context) {
		gn := goon.FromContext(c)
		o := struct {
			Opml    []*OpmlOutline
			Stories map[string][]*Story
			Icons   map[string]string
			NoAds   map[string]bool
			Options string
		}{
			Opml:    uf.Outline,
			Stories: fl,
			Icons:   icons,
			NoAds:   noads,
			Options: u.Options,
		}
		b, err := json.Marshal(o)
		if err != nil {
			c.Errorf("cleaning")
			for _, v := range fl {
				for _, s := range v {
					n := sanitizer.CleanNonUTF8(s.Summary)
					if n != s.Summary {
						s.Summary = n
						c.Errorf("cleaned %v", s.Id)
						gn.Put(s)
					}
				}
			}
			b, _ = json.Marshal(o)
		}
		w.Write(b)
	})
}

func MarkRead(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	cu := user.Current(c)
	gn := goon.FromContext(c)
	read := make(Read)
	var stories []readStory
	defer r.Body.Close()
	b, _ := ioutil.ReadAll(r.Body)
	if err := json.Unmarshal(b, &stories); err != nil {
		serveError(w, err)
		return
	}
	gn.RunInTransaction(func(gn *goon.Goon) error {
		u := &User{Id: cu.ID}
		ud := &UserData{
			Id:     "data",
			Parent: gn.Key(u),
		}
		if err := gn.Get(ud); err != nil {
			return err
		}
		gob.NewDecoder(bytes.NewReader(ud.Read)).Decode(&read)
		for _, s := range stories {
			read[s] = true
		}
		var b bytes.Buffer
		gob.NewEncoder(&b).Encode(&read)
		ud.Read = b.Bytes()
		_, err := gn.Put(ud)
		return err
	}, nil)
}

func MarkUnread(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	cu := user.Current(c)
	gn := goon.FromContext(c)
	read := make(Read)
	f := r.FormValue("feed")
	s := r.FormValue("story")
	rs := readStory{Feed: f, Story: s}
	u := &User{Id: cu.ID}
	ud := &UserData{
		Id:     "data",
		Parent: gn.Key(u),
	}
	gn.RunInTransaction(func(gn *goon.Goon) error {
		if err := gn.Get(ud); err != nil {
			return err
		}
		gob.NewDecoder(bytes.NewReader(ud.Read)).Decode(&read)
		delete(read, rs)
		b := bytes.Buffer{}
		gob.NewEncoder(&b).Encode(&read)
		ud.Read = b.Bytes()
		_, err := gn.Put(ud)
		return err
	}, nil)
}

func GetContents(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	var reqs []struct {
		Feed  string
		Story string
	}
	defer r.Body.Close()
	b, _ := ioutil.ReadAll(r.Body)
	if err := json.Unmarshal(b, &reqs); err != nil {
		serveError(w, err)
		return
	}
	scs := make([]*StoryContent, len(reqs))
	gn := goon.FromContext(c)
	for i, r := range reqs {
		f := &Feed{Url: r.Feed}
		s := &Story{Id: r.Story, Parent: gn.Key(f)}
		scs[i] = &StoryContent{Id: 1, Parent: gn.Key(s)}
	}
	gn.GetMulti(scs)
	ret := make([]string, len(reqs))
	for i, sc := range scs {
		ret[i] = sc.content()
	}
	b, _ = json.Marshal(&ret)
	w.Write(b)
}

func ClearFeeds(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	if !isDevServer {
		return
	}

	cu := user.Current(c)
	gn := goon.FromContext(c)
	done := make(chan bool)
	go func() {
		u := &User{Id: cu.ID}
		defer func() { done <- true }()
		ud := &UserData{Id: "data", Parent: gn.Key(u)}
		if err := gn.Get(u); err != nil {
			c.Errorf("user del err: %v", err.Error())
			return
		}
		gn.Get(ud)
		u.Read = time.Time{}
		ud.Read = nil
		ud.Opml = nil
		gn.PutMany(u, ud)
		c.Infof("%v cleared", u.Email)
	}()
	del := func(kind string) {
		defer func() { done <- true }()
		q := datastore.NewQuery(kind).KeysOnly()
		keys, err := gn.GetAll(q, nil)
		if err != nil {
			c.Errorf("err: %v", err.Error())
			return
		}
		if err := gn.DeleteMulti(keys); err != nil {
			c.Errorf("err: %v", err.Error())
			return
		}
		c.Infof("%v deleted", kind)
	}
	types := []interface{}{
		&Feed{},
		&Story{},
		&StoryContent{},
		&DateFormat{},
		&Log{},
		&UserOpml{},
	}
	for _, i := range types {
		k := gn.Key(i).Kind()
		go del(k)
	}

	for i := 0; i < len(types); i++ {
		<-done
	}

	http.Redirect(w, r, routeUrl("main"), http.StatusFound)
}

func ExportOpml(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	cu := user.Current(c)
	gn := goon.FromContext(c)
	u := User{Id: cu.ID}
	ud := UserData{Id: "data", Parent: gn.Key(&User{Id: cu.ID})}
	if err := gn.Get(&u); err != nil {
		serveError(w, err)
		return
	}
	gn.Get(&ud)
	downloadOpml(w, ud.Opml, u.Email)
}

func downloadOpml(w http.ResponseWriter, ob []byte, email string) {
	opml := Opml{}
	json.Unmarshal(ob, &opml)
	opml.Version = "1.0"
	opml.Title = fmt.Sprintf("%s subscriptions in Go Read", email)
	for _, o := range opml.Outline {
		o.Text = o.Title
		if len(o.XmlUrl) > 0 {
			o.Type = "rss"
		}
		for _, so := range o.Outline {
			so.Text = so.Title
			so.Type = "rss"
		}
	}
	b, _ := xml.MarshalIndent(&opml, "", "\t")
	w.Header().Add("Content-Type", "text/xml")
	w.Header().Add("Content-Disposition", "attachment; filename=subscriptions.opml")
	fmt.Fprint(w, xml.Header, string(b))
}

func UploadOpml(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	opml := Opml{}
	if err := json.Unmarshal([]byte(r.FormValue("opml")), &opml.Outline); err != nil {
		serveError(w, err)
		return
	}
	backupOPML(c)
	cu := user.Current(c)
	gn := goon.FromContext(c)
	u := User{Id: cu.ID}
	ud := UserData{Id: "data", Parent: gn.Key(&u)}
	if err := gn.Get(&ud); err != nil {
		serveError(w, err)
		c.Errorf("get err: %v", err)
		return
	}
	if b, err := json.Marshal(&opml); err != nil {
		serveError(w, err)
		c.Errorf("json err: %v", err)
		return
	} else {
		l := Log{
			Parent: ud.Parent,
			Id:     time.Now().UnixNano(),
			Text:   fmt.Sprintf("upload opml: %v -> %v", len(ud.Opml), len(b)),
		}
		ud.Opml = b
		if _, err := gn.PutMany(&ud, &l); err != nil {
			serveError(w, err)
			return
		}
		backupOPML(c)
	}
}

func backupOPML(c mpg.Context) {
	cu := user.Current(c)
	gn := goon.FromContext(c)
	u := User{Id: cu.ID}
	ud := UserData{Id: "data", Parent: gn.Key(&u)}
	if err := gn.Get(&ud); err != nil {
		return
	}
	uo := UserOpml{Id: time.Now().UnixNano(), Parent: gn.Key(&u)}
	buf := &bytes.Buffer{}
	if gz, err := gzip.NewWriterLevel(buf, gzip.BestCompression); err == nil {
		gz.Write([]byte(ud.Opml))
		gz.Close()
		uo.Compressed = buf.Bytes()
	} else {
		c.Errorf("gz err: %v", err)
		uo.Opml = ud.Opml
	}
	gn.Put(&uo)
}

func FeedHistory(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	cu := user.Current(c)
	gn := goon.FromContext(c)
	u := User{Id: cu.ID}
	uk := gn.Key(&u)
	if v := r.FormValue("v"); len(v) == 0 {
		q := datastore.NewQuery(gn.Key(&UserOpml{}).Kind()).Ancestor(uk).KeysOnly()
		keys, err := gn.GetAll(q, nil)
		if err != nil {
			serveError(w, err)
			return
		}
		times := make([]string, len(keys))
		for i, k := range keys {
			times[i] = strconv.FormatInt(k.IntID(), 10)
		}
		b, _ := json.Marshal(&times)
		w.Write(b)
	} else {
		a, _ := strconv.ParseInt(v, 10, 64)
		uo := UserOpml{Id: a, Parent: uk}
		if err := gn.Get(&uo); err != nil {
			serveError(w, err)
			return
		}
		downloadOpml(w, uo.opml(), cu.Email)
	}
}

func SaveOptions(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	cu := user.Current(c)
	gn := goon.FromContext(c)
	gn.RunInTransaction(func(gn *goon.Goon) error {
		u := User{Id: cu.ID}
		if err := gn.Get(&u); err != nil {
			serveError(w, err)
			return nil
		}
		u.Options = r.FormValue("options")
		_, err := gn.PutMany(&u, &Log{
			Parent: gn.Key(&u),
			Id:     time.Now().UnixNano(),
			Text:   fmt.Sprintf("save options: %v", len(u.Options)),
		})
		return err
	}, nil)
}

func GetFeed(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	gn := goon.FromContext(c)
	f := Feed{Url: r.FormValue("f")}
	fk := gn.Key(&f)
	q := datastore.NewQuery(gn.Key(&Story{}).Kind()).Ancestor(fk).KeysOnly()
	q = q.Order("-" + IDX_COL)
	if c := r.FormValue("c"); c != "" {
		if dc, err := datastore.DecodeCursor(c); err == nil {
			q = q.Start(dc)
		}
	}
	iter := gn.Run(q)
	var stories []*Story
	for i := 0; i < 20; i++ {
		if k, err := iter.Next(nil); err == nil {
			stories = append(stories, &Story{
				Id:     k.StringID(),
				Parent: k.Parent(),
			})
		} else if err == datastore.Done {
			break
		} else {
			serveError(w, err)
			return
		}
	}
	cursor := ""
	if ic, err := iter.Cursor(); err == nil {
		cursor = ic.String()
	}
	gn.GetMulti(&stories)
	b, _ := json.Marshal(struct {
		Cursor  string
		Stories []*Story
	}{
		Cursor:  cursor,
		Stories: stories,
	})
	w.Write(b)
}

func DeleteAccount(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	if _, err := doUncheckout(c); err != nil {
		c.Errorf("uncheckout err: %v", err)
	}
	cu := user.Current(c)
	gn := goon.FromContext(c)
	u := User{Id: cu.ID}
	ud := UserData{Id: "data", Parent: gn.Key(&u)}
	if err := gn.Get(&u); err != nil {
		serveError(w, err)
		return
	}
	gn.Delete(gn.Key(&ud))
	gn.Delete(ud.Parent)
	http.Redirect(w, r, routeUrl("logout"), http.StatusFound)
}
