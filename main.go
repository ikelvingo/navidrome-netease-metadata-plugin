package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/metadata"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
)

var (
	_ metadata.ArtistURLProvider       = (*neteaseMetadataProvider)(nil)
	_ metadata.ArtistBiographyProvider = (*neteaseMetadataProvider)(nil)
	_ metadata.ArtistImagesProvider    = (*neteaseMetadataProvider)(nil)
	_ metadata.ArtistTopSongsProvider  = (*neteaseMetadataProvider)(nil)
	_ metadata.AlbumInfoProvider       = (*neteaseMetadataProvider)(nil)
	_ metadata.AlbumImagesProvider     = (*neteaseMetadataProvider)(nil)
)

const httpTimeoutMs int32 = 15000

var (
	ErrNotFound    = errors.New("netease: not found")
	ErrAPIError    = errors.New("netease: api error")
	ErrInvalidCode = errors.New("netease: invalid response code")
)

type LoadBalanceMode int

const (
	LoadBalanceModeRandom     LoadBalanceMode = iota
	LoadBalanceModeRoundRobin LoadBalanceMode = iota
)

type neteaseClient struct {
	baseURLs        []string
	loadBalanceMode LoadBalanceMode
	currentIndex    uint64
}

func newClient() *neteaseClient {
	c := &neteaseClient{
		baseURLs: getDefaultAPIBaseURLs(),
	}
	if modeStr, ok := pdk.GetConfig("LoadBalanceMode"); ok {
		if modeStr == "roundrobin" {
			c.loadBalanceMode = LoadBalanceModeRoundRobin
		}
	}
	if urlsStr, ok := pdk.GetConfig("APIUrls"); ok && urlsStr != "" {
		parts := strings.Split(urlsStr, ",")
		var urls []string
		for _, p := range parts {
			u := strings.TrimSpace(p)
			if u != "" {
				urls = append(urls, u)
			}
		}
		if len(urls) > 0 {
			c.baseURLs = urls
		}
	}
	return c
}

func getDefaultAPIBaseURLs() []string {
	return []string{
		"https://apis.netstart.cn/music",
		"https://ncm.zhenxin.me",
		"https://ncmapi.btwoa.com",
		"https://zm.wwoyun.cn",
	}
}

func (c *neteaseClient) getBaseURL() string {
	if len(c.baseURLs) == 0 {
		return getDefaultAPIBaseURLs()[0]
	}
	if len(c.baseURLs) == 1 {
		return c.baseURLs[0]
	}
	switch c.loadBalanceMode {
	case LoadBalanceModeRoundRobin:
		idx := atomic.AddUint64(&c.currentIndex, 1)
		return c.baseURLs[int(idx)%len(c.baseURLs)]
	case LoadBalanceModeRandom:
		fallthrough
	default:
		return c.baseURLs[rand.Intn(len(c.baseURLs))]
	}
}

func httpGetJSON(rawURL string, target any) error {
	resp, err := host.HTTPSend(host.HTTPRequest{
		Method:    "GET",
		URL:       rawURL,
		TimeoutMs: httpTimeoutMs,
		Headers: map[string]string{
			"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 Edg/129.0.0.0",
			"Origin":     "https://music.163.com",
			"Referer":    "https://music.163.com",
		},
	})
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(resp.Body, target); err != nil {
		return fmt.Errorf("json decode: %w", err)
	}
	return nil
}

type searchResult struct {
	Result struct {
		Artists     []neteaseArtist `json:"artists"`
		ArtistCount int             `json:"artistCount"`
		Songs       []neteaseSong   `json:"songs"`
		SongCount   int             `json:"songCount"`
		Albums      []neteaseAlbum  `json:"albums"`
		AlbumCount  int             `json:"albumCount"`
	} `json:"result"`
	Code int `json:"code"`
}

type neteaseArtist struct {
	ID        int      `json:"id"`
	Name      string   `json:"name"`
	PicURL    string   `json:"picUrl"`
	Img1v1URL string   `json:"img1v1Url"`
	Alias     []string `json:"alias"`
	Trans     string   `json:"trans"`
	BriefDesc string   `json:"briefDesc"`
}

type neteaseSong struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Ar   []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"ar"`
	Al struct {
		ID     int    `json:"id"`
		Name   string `json:"name"`
		PicURL string `json:"picUrl"`
	} `json:"al"`
	Dt int `json:"dt"`
}

type neteaseAlbum struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	PicURL      string `json:"picUrl"`
	BlurPicURL  string `json:"blurPicUrl"`
	Artist      struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"artist"`
	Description string `json:"description"`
}

type artistDetail struct {
	Data struct {
		Artist neteaseArtist `json:"artist"`
	} `json:"data"`
	Code int `json:"code"`
}

type artistDesc struct {
	BriefDesc    string `json:"briefDesc"`
	Introduction []struct {
		Ti  string `json:"ti"`
		Txt string `json:"txt"`
	} `json:"introduction"`
	Code int `json:"code"`
}

type artistTopSongsV2 struct {
	Songs []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		Ar   []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"ar"`
		Dt int `json:"dt"`
	} `json:"songs"`
	More bool `json:"more"`
	Code int  `json:"code"`
}

type albumDetail struct {
	Album neteaseAlbum `json:"album"`
	Songs []neteaseSong `json:"songs"`
	Code  int          `json:"code"`
}

type neteaseMetadataProvider struct {
	client *neteaseClient
}

func (p *neteaseMetadataProvider) GetArtistURL(req metadata.ArtistRequest) (*metadata.ArtistURLResponse, error) {
	artists, err := p.searchArtists(req.Name, 1)
	if err != nil || len(artists) == 0 {
		pdk.Log(pdk.LogDebug, "netease: GetArtistURL search failed: "+fmt.Sprint(err))
		return nil, nil
	}
	u := fmt.Sprintf("https://music.163.com/#/artist?id=%d", artists[0].ID)
	pdk.Log(pdk.LogDebug, "netease: GetArtistURL success: "+u)
	return &metadata.ArtistURLResponse{URL: u}, nil
}

func (p *neteaseMetadataProvider) GetArtistBiography(req metadata.ArtistRequest) (*metadata.ArtistBiographyResponse, error) {
	artists, err := p.searchArtists(req.Name, 1)
	if err != nil || len(artists) == 0 {
		pdk.Log(pdk.LogDebug, "netease: GetArtistBiography search failed: "+fmt.Sprint(err))
		return nil, nil
	}

	var desc artistDesc
	descURL := fmt.Sprintf("%s/artist/desc?id=%s", p.client.getBaseURL(), url.QueryEscape(strconv.Itoa(artists[0].ID)))
	pdk.Log(pdk.LogDebug, "netease: fetching biography: "+descURL)
	if err := httpGetJSON(descURL, &desc); err != nil {
		pdk.Log(pdk.LogDebug, "netease: /artist/desc failed: "+fmt.Sprint(err))
		return nil, nil
	}
	pdk.Log(pdk.LogDebug, fmt.Sprintf("netease: /artist/desc code=%d briefDesc=%q", desc.Code, desc.BriefDesc))
	if desc.Code != 200 {
		return nil, nil
	}

	var bio strings.Builder
	if desc.BriefDesc != "" {
		bio.WriteString(htmlEscape(desc.BriefDesc))
	}
	for _, intro := range desc.Introduction {
		if intro.Ti != "" {
			bio.WriteString("<br><br><b>")
			bio.WriteString(htmlEscape(intro.Ti))
			bio.WriteString("</b><br>")
		}
		if intro.Txt != "" {
			text := htmlEscape(intro.Txt)
			text = strings.ReplaceAll(text, "\\n", "<br>")
			text = strings.ReplaceAll(text, "\n", "<br>")
			bio.WriteString(text)
		}
	}

	result := strings.TrimSpace(bio.String())
	if result == "" {
		pdk.Log(pdk.LogDebug, "netease: GetArtistBiography empty result")
		return nil, nil
	}
	pdk.Log(pdk.LogDebug, fmt.Sprintf("netease: GetArtistBiography success, %d chars", len(result)))
	return &metadata.ArtistBiographyResponse{Biography: result}, nil
}

func (p *neteaseMetadataProvider) GetArtistTopSongs(req metadata.TopSongsRequest) (*metadata.TopSongsResponse, error) {
	artists, err := p.searchArtists(req.Name, 1)
	if err != nil || len(artists) == 0 {
		return nil, nil
	}

	var topSongs artistTopSongsV2
	topURL := fmt.Sprintf("%s/artist/top/song?id=%s", p.client.getBaseURL(), url.QueryEscape(strconv.Itoa(artists[0].ID)))
	if err := httpGetJSON(topURL, &topSongs); err != nil {
		return nil, nil
	}
	if topSongs.Code != 200 || len(topSongs.Songs) == 0 {
		return nil, nil
	}

	count := int(req.Count)
	if count <= 0 || count > len(topSongs.Songs) {
		count = len(topSongs.Songs)
	}

	songs := make([]metadata.SongRef, 0, count)
	for _, s := range topSongs.Songs {
		if len(songs) >= count {
			break
		}
		artistNames := make([]string, 0, len(s.Ar))
		for _, ar := range s.Ar {
			artistNames = append(artistNames, ar.Name)
		}
		songs = append(songs, metadata.SongRef{
			Name:     s.Name,
			Artist:   strings.Join(artistNames, ", "),
			Duration: float32(s.Dt) / 1000,
		})
	}

	return &metadata.TopSongsResponse{Songs: songs}, nil
}

func (p *neteaseMetadataProvider) GetArtistImages(req metadata.ArtistRequest) (*metadata.ArtistImagesResponse, error) {
	artists, err := p.searchArtists(req.Name, 1)
	if err != nil || len(artists) == 0 {
		pdk.Log(pdk.LogDebug, "netease: GetArtistImages search failed: "+fmt.Sprint(err))
		return nil, nil
	}

	artist := artists[0]
	picURL := artist.PicURL
	img1v1URL := artist.Img1v1URL

	pdk.Log(pdk.LogDebug, fmt.Sprintf("netease: artist picUrl=%q img1v1Url=%q", picURL, img1v1URL))

	if picURL == "" {
		var detail artistDetail
		detURL := fmt.Sprintf("%s/artist/detail?id=%s", p.client.getBaseURL(), url.QueryEscape(strconv.Itoa(artist.ID)))
		if err := httpGetJSON(detURL, &detail); err == nil && detail.Code == 200 {
			if detail.Data.Artist.PicURL != "" {
				picURL = detail.Data.Artist.PicURL
			}
		}
	}

	var images []metadata.ImageInfo
	if picURL != "" {
		images = append(images, metadata.ImageInfo{URL: picURL, Size: 640})
	}
	if img1v1URL != "" && img1v1URL != picURL {
		images = append(images, metadata.ImageInfo{URL: img1v1URL, Size: 300})
	}

	if len(images) == 0 {
		pdk.Log(pdk.LogDebug, "netease: GetArtistImages no images")
		return nil, nil
	}
	return &metadata.ArtistImagesResponse{Images: images}, nil
}

func (p *neteaseMetadataProvider) GetAlbumInfo(req metadata.AlbumRequest) (*metadata.AlbumInfoResponse, error) {
	albums, err := p.searchAlbums(req.Name, 1)
	if err != nil || len(albums) == 0 {
		return nil, nil
	}

	var detail albumDetail
	detURL := fmt.Sprintf("%s/album?id=%s", p.client.getBaseURL(), url.QueryEscape(strconv.Itoa(albums[0].ID)))
	if err := httpGetJSON(detURL, &detail); err != nil {
		return nil, nil
	}
	if detail.Code != 200 {
		return nil, nil
	}

	album := detail.Album
	return &metadata.AlbumInfoResponse{
		Name:        album.Name,
		Description: album.Description,
		URL:         fmt.Sprintf("https://music.163.com/#/album?id=%d", album.ID),
	}, nil
}

func (p *neteaseMetadataProvider) GetAlbumImages(req metadata.AlbumRequest) (*metadata.AlbumImagesResponse, error) {
	albums, err := p.searchAlbums(req.Name, 1)
	if err != nil || len(albums) == 0 {
		return nil, nil
	}

	album := albums[0]
	var images []metadata.ImageInfo
	if album.PicURL != "" {
		images = append(images, metadata.ImageInfo{URL: album.PicURL, Size: 640})
	}
	if album.BlurPicURL != "" && album.BlurPicURL != album.PicURL {
		images = append(images, metadata.ImageInfo{URL: album.BlurPicURL, Size: 300})
	}

	if len(images) == 0 {
		return nil, nil
	}
	return &metadata.AlbumImagesResponse{Images: images}, nil
}

func (p *neteaseMetadataProvider) searchArtists(name string, limit int) ([]neteaseArtist, error) {
	var result searchResult
	sURL := fmt.Sprintf("%s/cloudsearch?keywords=%s&type=100&limit=%d",
		p.client.getBaseURL(), url.QueryEscape(name), limit)
	pdk.Log(pdk.LogDebug, "netease: searching artist: "+sURL)
	if err := httpGetJSON(sURL, &result); err != nil {
		pdk.Log(pdk.LogDebug, "netease: search request failed: "+fmt.Sprint(err))
		return nil, err
	}
	pdk.Log(pdk.LogDebug, fmt.Sprintf("netease: search code=%d hits=%d", result.Code, len(result.Result.Artists)))
	if result.Code != 200 {
		return nil, fmt.Errorf("%w: code %d", ErrInvalidCode, result.Code)
	}
	if len(result.Result.Artists) == 0 {
		return nil, ErrNotFound
	}
	return result.Result.Artists, nil
}

func (p *neteaseMetadataProvider) searchAlbums(name string, limit int) ([]neteaseAlbum, error) {
	var result searchResult
	sURL := fmt.Sprintf("%s/cloudsearch?keywords=%s&type=10&limit=%d",
		p.client.getBaseURL(), url.QueryEscape(name), limit)
	if err := httpGetJSON(sURL, &result); err != nil {
		return nil, err
	}
	if result.Code != 200 {
		return nil, fmt.Errorf("%w: code %d", ErrInvalidCode, result.Code)
	}
	if len(result.Result.Albums) == 0 {
		return nil, ErrNotFound
	}
	return result.Result.Albums, nil
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

func init() {
	impl := &neteaseMetadataProvider{client: newClient()}
	metadata.Register(impl)
}

func main() {}

