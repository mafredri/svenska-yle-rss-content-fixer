package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gorilla/feeds"
	"github.com/mmcdole/gofeed"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

const (
	userAgent   = "svenska-yle-rss-content-fixer/0.1"
	maxFeedSize = 5 * 1024 * 1024 // 5 MB.
	maxBodySize = 5 * 1024 * 1024 // 5 MB.
	maxWorkers  = 5

	rssBaseURL = "https://svenska.yle.fi/rss"
)

func main() {
	bind := flag.String("bind", "127.0.0.1", "Listen to requests on this interface")
	port := flag.Int("port", 8080, "Port to listen to")
	flag.Parse()

	addr := fmt.Sprintf("%s:%d", *bind, *port)
	log.Printf("Listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, http.HandlerFunc(handler)))
}

func handler(w http.ResponseWriter, r *http.Request) {
	rssURL := fmt.Sprintf("%s%s", rssBaseURL, r.URL.Path)
	log.Printf("Serving RSS: %s", rssURL)

	rss, err := fetchRSS(r.Context(), rssURL)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Printf("error serving request: %v", err)
		return
	}
	w.Write(rss)
}

var articleCache = &sync.Map{} // map[articleKey]string

// Cache on GUID (URL) and update timestamp so articles can be refreshed.
type articleKey struct {
	guid    string
	updated time.Time
}

func newArticleKey(item *gofeed.Item) articleKey {
	t := item.PublishedParsed
	if item.UpdatedParsed != nil {
		t = item.UpdatedParsed
	}
	return articleKey{guid: item.GUID, updated: *t}
}

func fetchRSS(ctx context.Context, url string) ([]byte, error) {
	client := http.DefaultClient
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http status failed: %d", resp.StatusCode)
	}

	r := io.LimitReader(resp.Body, maxFeedSize)
	fp := gofeed.NewParser()
	feed, err := fp.Parse(r)
	if err != nil {
		return nil, err
	}

	sem := semaphore.NewWeighted(maxWorkers)
	eg, egCtx := errgroup.WithContext(ctx)
	sf := singleflight.Group{}
	currentGUIDs := make(map[string]bool)
	for i, item := range feed.Items {
		currentGUIDs[item.GUID] = true

		i := i
		item := item
		eg.Go(func() error {
			_, err, _ := sf.Do(item.GUID, func() (interface{}, error) {
				key := newArticleKey(item)
				if a, ok := articleCache.Load(key); ok {
					item.Content = a.(string)
					return nil, nil
				}

				req, err := http.NewRequestWithContext(egCtx, "GET", item.Link, nil)
				if err != nil {
					log.Printf("Skipping item %d: %v", i, err)
					return nil, nil
				}

				if err := sem.Acquire(egCtx, 1); err != nil {
					return nil, err
				}
				defer sem.Release(1)

				resp, err := client.Do(req)
				if err != nil {
					log.Printf("Skipping item %d: %v", i, err)
					return nil, nil
				}

				if err = func() error {
					defer resp.Body.Close()
					r := io.LimitReader(resp.Body, maxBodySize)
					doc, err := goquery.NewDocumentFromReader(r)
					if err != nil {
						return err
					}

					sel := doc.Find("#main-content")

					// Clean up content.
					sel.Find(".ydd-article-headline").Remove()
					sel.Find(".ydd-articles-list").Remove()
					sel.Find(".ydd-share-buttons").Remove()
					sel.Find("#comments").Remove()

					// Fix image SRCs, avoid Yle cropping service because it's slow.
					for _, img := range sel.Find("img").Nodes {
						var content string
						for i := range img.Attr {
							if img.Attr[i].Key == "content" {
								content = img.Attr[i].Val
							}
						}

						// https://images.cdn.yle.fi/image/upload/f_auto,fl_progressive/q_88/w_4819,h_2711,c_crop,x_431,y_422/w_1200/v1622036527/39-81151860ae4f34508bf.jpg
						// =>
						// https://images.cdn.yle.fi/image/upload/v1622036527/39-81151860ae4f34508bf.jpg
						parts := strings.Split(content, "/")
						parts = append(parts[0:5], parts[len(parts)-2:]...)
						content = strings.Join(parts, "/")

						for i := range img.Attr {
							if img.Attr[i].Key == "src" {
								img.Attr[i].Val = content
							}
						}
					}
					content, err := sel.Html()
					if err != nil {
						return err
					}

					articleCache.Store(key, content)

					return nil
				}(); err != nil {
					log.Printf("Skipping item %d: %v", i, err)
					return nil, nil
				}

				return nil, nil
			})
			if err != nil {
				return err
			}

			return nil
		})
	}

	// Clear stale cache while we're waiting.
	articleCache.Range(func(key, value interface{}) bool {
		k := key.(articleKey)
		if !currentGUIDs[k.guid] {
			log.Printf("Article %s expired, removing from cache", k.guid)
			articleCache.Delete(k)
		}
		return true
	})

	if err = eg.Wait(); err != nil {
		return nil, err
	}

	newFeed := &feeds.Feed{
		Title:       feed.Title,
		Link:        &feeds.Link{Href: feed.FeedLink},
		Description: feed.Description,
		Updated:     *feed.UpdatedParsed,
	}
	if len(feed.Authors) > 0 {
		newFeed.Author = &feeds.Author{
			Name:  feed.Authors[0].Name,
			Email: feed.Authors[0].Email,
		}
	}
	if feed.PublishedParsed != nil {
		newFeed.Created = *feed.PublishedParsed
	}
	if feed.UpdatedParsed != nil {
		newFeed.Updated = *feed.UpdatedParsed
	}

	for _, item := range feed.Items {
		key := newArticleKey(item)
		var content string
		if c, ok := articleCache.Load(key); ok {
			content = c.(string)
		}

		newItem := &feeds.Item{
			Title:       item.Title,
			Link:        &feeds.Link{Href: item.Link},
			Description: item.Description,
			Id:          item.GUID,
			Content:     content,
		}
		if len(item.Authors) > 0 {
			newItem.Author = &feeds.Author{
				Name:  item.Authors[0].Name,
				Email: item.Authors[0].Email,
			}
		}
		if item.PublishedParsed != nil {
			newItem.Created = *item.PublishedParsed
		}
		if item.UpdatedParsed != nil {
			newItem.Updated = *item.UpdatedParsed
		}
		newFeed.Items = append(newFeed.Items, newItem)
	}

	rss, err := newFeed.ToRss()
	if err != nil {
		return nil, err
	}

	return []byte(rss), nil
}
