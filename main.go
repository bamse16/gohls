/*

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU General Public License as published by
   the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU General Public License for more details.

   You should have received a copy of the GNU General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.

*/

package main

import "flag"
import "fmt"
import "io"
import "net/http"
import "net/url"
import "log"
import "os"
import "time"
import "github.com/golang/groupcache/lru"
import "strings"
import "github.com/kz26/m3u8"

const version = "1.1.0"

var userAgent string

var client = &http.Client{}

func doRequest(c *http.Client, req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	return resp, err
}

// Download stores URI/duration to process
type Download struct {
	URI           string
	totalDuration time.Duration
}

type stream struct {
	URI       string
	localFile string
}

func downloadSegment(fn string, dlc chan *Download) {
	out, err := os.OpenFile(fn, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)

	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()
	for v := range dlc {
		onDownload(v, out)
	}
}

func onDownload(v *Download, out *os.File) {
	req, err := http.NewRequest("GET", v.URI, nil)
	if err != nil {
		log.Fatal(err)
	}
	resp, err := doRequest(client, req)
	if err != nil {
		log.Print(err)
		return
	}
	if resp.StatusCode != 200 {
		log.Printf("Received HTTP %v for %v\n", resp.StatusCode, v.URI)
		return
	}
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	resp.Body.Close()
	log.Printf("Downloaded %v. Recorded %v.\n", v.URI, v.totalDuration)
}

func downloadURI(v *stream, out *os.File) {
	req, err := http.NewRequest("GET", v.URI, nil)
	if err != nil {
		log.Fatal(err)
	}
	resp, err := doRequest(client, req)
	defer resp.Body.Close()
	if err != nil {
		log.Print(err)
		return
	}
	if resp.StatusCode != 200 {
		log.Printf("Received HTTP %v for %v.\n", resp.StatusCode, v.URI)
		return
	}
	log.Printf("Downloading %v to %v.\n", v.URI, v.localFile)
	written, err := io.Copy(out, resp.Body)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Downloaded %v kb from %v.\n", written/1000, v.URI)
}

func downloadStream(s *stream) {
	if downloadInProgress(s.localFile) {
		log.Printf("Download in progress for %v.\n", s)
		return
	}

	out, err := os.OpenFile(s.localFile, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		log.Fatal(err)
	}

	shouldWait := false
	shortSleepInterval := time.Duration(1) * time.Second
	longSleepInterval := time.Duration(10) * time.Second

	shortTicks := 0
	longTicks := 0

	maxTicks := 30

	for {
		req, err := http.NewRequest("GET", s.URI, nil)
		if err != nil {
			log.Fatal(err)
		}
		resp, err := doRequest(client, req)
		defer resp.Body.Close()

		// If provided url is already a stream, just save it
		if isAudioStream(resp) {
			shouldWait = true
			shortTicks = 0
			longTicks = 0
			downloadURI(s, out)
		} else {

			sleepInterval := longSleepInterval
			if shortTicks < maxTicks {
				shortTicks = shortTicks + 1
				sleepInterval = shortSleepInterval
			} else if longTicks < maxTicks {
				longTicks = longTicks + 1
			} else {
				break // Break after longTicks > maxTicks
			}

			if shouldWait {
				log.Printf("Sleeping for %v.", sleepInterval)
			} else {
				log.Print("URL not a stream. Bailing.")
				break
			}
			time.Sleep(sleepInterval)
		}
	}
}

func downloadInProgress(fn string) bool {
	inProgress := false

	info, err := os.Stat(fn)
	if os.IsNotExist(err) {
		return inProgress
	}
	if err != nil {
		log.Printf("Could not get stats for %v. %v", fn, err)
		return inProgress
	}

	delta := time.Now().Sub(info.ModTime())
	inProgress = delta < time.Duration(5)*time.Minute

	log.Printf("File %v modified %v ago.\n", fn, delta)

	return inProgress
}

func getPlaylist(urlStr string, useLocalTime bool, dlc chan *Download) {
	startTime := time.Now()
	var recDuration time.Duration
	cache := lru.New(1024)
	playlistURL, err := url.Parse(urlStr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		req, err := http.NewRequest("GET", urlStr, nil)
		if err != nil {
			log.Fatal(err)
		}
		resp, err := doRequest(client, req)

		// If provided url is already a stream, just save it
		if isAudioStream(resp) {
			resp.Body.Close()
			recDuration := 12 * time.Hour
			dlc <- &Download{urlStr, recDuration}
			return
		}

		if err != nil {
			log.Print(err)
			time.Sleep(time.Duration(3) * time.Second)
		}
		playlist, listType, err := m3u8.DecodeFrom(resp.Body, true)
		if err != nil {
			log.Fatal(err)
		}
		resp.Body.Close()
		if listType == m3u8.MEDIA {
			mpl := playlist.(*m3u8.MediaPlaylist)
			for _, v := range mpl.Segments {
				if v != nil {
					var msURI string
					if strings.HasPrefix(v.URI, "http") {
						msURI, err = url.QueryUnescape(v.URI)
						if err != nil {
							log.Fatal(err)
						}
					} else {
						msURL, err := playlistURL.Parse(v.URI)
						if err != nil {
							log.Print(err)
							continue
						}
						msURI, err = url.QueryUnescape(msURL.String())
						if err != nil {
							log.Fatal(err)
						}
					}
					_, hit := cache.Get(msURI)
					if !hit {
						cache.Add(msURI, nil)
						if useLocalTime {
							recDuration = time.Now().Sub(startTime)
						} else {
							recDuration += time.Duration(int64(v.Duration * 1000000000))
						}
						dlc <- &Download{msURI, recDuration}
					}
				}
			}
			if mpl.Closed {
				close(dlc)
				return
			}

			time.Sleep(time.Duration(int64(mpl.TargetDuration * 1000000000)))

		} else {
			log.Fatal("Not a valid media playlist")
		}
	}
}

func debugResponse(r *http.Response) string {
	var request []string

	request = append(request, "Headers:")

	// Headers
	for name, headers := range r.Header {
		name = strings.ToLower(name)
		for _, h := range headers {
			request = append(request, fmt.Sprintf("%v: %v", name, h))
		}
	}

	request = append(request, fmt.Sprintf("Status: %v", r.StatusCode))

	return strings.Join(request, "\n")
}

func isAudioStream(r *http.Response) bool {
	streams := []string{
		"audio/aacp",
		"audio/mpeg",
	}
	isStream := false

	for name, headers := range r.Header {
		name = strings.ToLower(name)
		if name != "content-type" {
			continue
		}

		for _, h := range headers {
			h = strings.ToLower(h)
			for _, stream := range streams {
				if stream == h {
					isStream = true
				}
			}
		}
	}

	return isStream
}

func main() {
	flag.StringVar(&userAgent, "ua", fmt.Sprintf("gohls/%v", version), "User-Agent for HTTP client")
	flag.Parse()

	os.Stderr.Write([]byte(fmt.Sprintf("gohls %v - HTTP Live Streaming (HLS) downloader\n", version)))
	os.Stderr.Write([]byte("Copyright (C) 2013-2014 Kevin Zhang. Licensed for use under the GNU GPL version 3.\n"))

	if flag.NArg() < 2 {
		os.Stderr.Write([]byte("Usage: gohls [-l=bool] [-t duration] [-ua user-agent] media-playlist-url output-file\n"))
		flag.PrintDefaults()
		os.Exit(2)
	}

	if !strings.HasPrefix(flag.Arg(0), "http") {
		log.Fatal("Media playlist url must begin with http/https")
	}

	s := stream{flag.Arg(0), flag.Arg(1)}
	downloadStream(&s)
}
