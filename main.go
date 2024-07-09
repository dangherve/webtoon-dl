package main

import (
    "archive/zip"
    "bytes"
    "encoding/json"
    "flag"
    "fmt"
    "github.com/anaskhan96/soup"
    "github.com/signintech/gopdf"
    "image"
    "io"
    "math"
    "net/http"
    "os"
    "regexp"
    "sort"
    "strconv"
    "strings"
    "time"
    "database/sql"
    _ "github.com/mattn/go-sqlite3"
    "log"
    "github.com/aherve/gopool"
    "net/url"
    "errors"
)
//    "unicode/utf8"
//    "sync"

var EpisodeGoroutine    *int
var WebtoonGoroutine    *int
var MaxWebtoonGoroutine *bool
var database            *bool
var FileVerify          *bool
var confOverride        *bool
var NoLog               *bool

type MotiontoonJson struct {
    Assets struct {
        Image map[string]string `json:"image"`
    } `json:"assets"`
}

type EpisodeBatch struct {
    imgLinks []string
    title    string
    minEp    int
    maxEp    int
}

type EpisodeInfo struct {
    title string
    url string
}

type ComicFile interface {
    addImage([]byte) error
    save(outFile string) error
}

type PDFComicFile struct {
    pdf *gopdf.GoPdf
}

// validate PDFComicFile implements ComicFile
var _ ComicFile = &PDFComicFile{}

func newPDFComicFile() *PDFComicFile {
    pdf := gopdf.GoPdf{}
    pdf.Start(gopdf.Config{Unit: gopdf.UnitPT, PageSize: *gopdf.PageSizeA4})
    return &PDFComicFile{pdf: &pdf}
}

func (c *PDFComicFile) addImage(img []byte) error {
    holder, err := gopdf.ImageHolderByBytes(img)
    if err != nil {
        return err
    }

    d, _, err := image.DecodeConfig(bytes.NewReader(img))
    if err != nil {
        return err
    }

    // gopdf assumes dpi 128 https://github.com/signintech/gopdf/issues/168
    // W and H are in points, 1 point = 1/72 inch
    // convert pixels (Width and Height) to points
    // subtract 1 point to account for margins
    c.pdf.AddPageWithOption(gopdf.PageOption{PageSize: &gopdf.Rect{
        W: float64(d.Width)*72/128 - 1,
        H: float64(d.Height)*72/128 - 1,
    }})
    return c.pdf.ImageByHolder(holder, 0, 0, nil)
}

func (c *PDFComicFile) save(outputPath string) error {
    return c.pdf.WritePdf(outputPath)
}

type CBZComicFile struct {
    zipWriter *zip.Writer
    buffer    *bytes.Buffer
    numFiles  int
}

// validate CBZComicFile implements ComicFile
var _ ComicFile = &CBZComicFile{}

func newCBZComicFile() (*CBZComicFile, error) {
    buffer := new(bytes.Buffer)
    zipWriter := zip.NewWriter(buffer)
    return &CBZComicFile{zipWriter: zipWriter, buffer: buffer, numFiles: 0}, nil
}

func (c *CBZComicFile) addImage(img []byte) error {
    f, err := c.zipWriter.Create(fmt.Sprintf("%010d.jpg", c.numFiles))
    if err != nil {
        return err
    }
    _, err = f.Write(img)
    if err != nil {
        return err
    }
    c.numFiles++
    return nil
}

func (c *CBZComicFile) save(outputPath string) error {
    if err := c.zipWriter.Close(); err != nil {
        return err
    }
    file, err := os.Create(outputPath)
    if err != nil {
        return err
    }
    defer func(file *os.File) {
        err := file.Close()
        if err != nil {
            fmt.Println(err.Error())
            os.Exit(1)
        }
    }(file)
    _, err = c.buffer.WriteTo(file)
    return err
}

func getOzPageImgLinks(doc soup.Root) []string {
    // regex find the documentURL, e.g:
    // viewerOptions: {
    //        // 필수항목
    //        containerId: '#ozViewer',
    //        documentURL: 'https://global.apis.naver.com/lineWebtoon/webtoon/motiontoonJson.json?seq=2830&hashValue=2e0b924676bdc38241bd8fd452191fe3',
    re := regexp.MustCompile("viewerOptions: \\{\n.*// 필수항목\n.*containerId: '#ozViewer',\n.*documentURL: '(.+)'")
    matches := re.FindStringSubmatch(doc.HTML())
    if len(matches) != 2 {
        fmt.Println("could not find documentURL")
        os.Exit(1)
    }

    // fetch json at documentURL and deserialize to MotiontoonJson
    resp, err := soup.Get(matches[1])
    if err != nil {
        fmt.Println(fmt.Sprintf("Error fetching page: %v", err))
        os.Exit(1)
    }
    var motionToon MotiontoonJson
    if err := json.Unmarshal([]byte(resp), &motionToon); err != nil {
        fmt.Println(fmt.Sprintf("Error unmarshalling json: %v", err))
        os.Exit(1)
    }

    // get sorted keys
    var sortedKeys []string
    for k := range motionToon.Assets.Image {
        sortedKeys = append(sortedKeys, k)
    }
    sort.Strings(sortedKeys)

    // get path rule, e.g:
    // motiontoonParam: {
    //   pathRuleParam: {os.Exit
    //     stillcut: 'https://ewebtoon-phinf.pstatic.net/motiontoon/3536_2e0b924676bdc38241bd8fd452191fe3/{=filename}?type=q70',
    re = regexp.MustCompile("motiontoonParam: \\{\n.*pathRuleParam: \\{\n.*stillcut: '(.+)'")
    matches = re.FindStringSubmatch(doc.HTML())
    if len(matches) != 2 {
        fmt.Println("could not find pathRule")
        os.Exit(1)
    }
    var imgs []string
    for _, k := range sortedKeys {
        imgs = append(imgs, strings.ReplaceAll(matches[1], "{=filename}", motionToon.Assets.Image[k]))
    }
    return imgs
}

func getImgLinksForEpisode(url string) []string {
    resp, err := soup.Get(url)
    time.Sleep(200 * time.Millisecond)
    if err != nil {
        fmt.Println(fmt.Sprintf("Error fetching page: %v", err))
        os.Exit(1)
    }
    doc := soup.HTMLParse(resp)
    imgs := doc.Find("div", "class", "viewer_lst").FindAll("img")
    if len(imgs) == 0 {
        // some comics seem to serve images from a different backend, something about oz
        return getOzPageImgLinks(doc)
    }
    var imgLinks []string
    for _, img := range imgs {
        if dataURL, ok := img.Attrs()["data-url"]; ok {
            imgLinks = append(imgLinks, dataURL)
        }
    }
    return imgLinks
}

func getEpisodeLinksForPage(url string) ([]EpisodeInfo, error) {
    resp, err := soup.Get(url)
    time.Sleep(200 * time.Millisecond)
    if err != nil {
        return []EpisodeInfo{}, fmt.Errorf("error fetching page: %v", err)
    }
    doc := soup.HTMLParse(resp)
    episodeURLs := doc.Find("div", "class", "detail_lst").FindAll("a")
    var episode []EpisodeInfo
//    var title string
    for _, episodeURL := range episodeURLs {
        if href := episodeURL.Attrs()["href"]; strings.Contains(href, "/viewer") {
            span:=episodeURL.Find("span","class","subj").Find("span")

//            title=span.Text()
            episode = append(episode, EpisodeInfo{
                title:span.Text(),
                url:href,
            })
        }
    }
    return episode, nil
}

func getEpisodeBatches(url string, minEp, maxEp, epsPerBatch int) ([]EpisodeBatch,error) {
    if strings.Contains(url, "/viewer") {
        // assume viewing single episode
        return []EpisodeBatch{{
            imgLinks: getImgLinksForEpisode(url),
            minEp:    episodeNo(url),
            maxEp:    episodeNo(url),
        }},nil
    } else {
        // assume viewing set of episodes
        log.Printf("scanning all pages to get all episode links")
        allEpisodeLinks := getAllEpisodeLinks(url)
        log.Printf("found %d total episodes", len(allEpisodeLinks))

        var desiredEpisodeLinks []string
        var desiredEpisodeTitles []string
        for _, episodeLink := range allEpisodeLinks {

            epNo := episodeNo(episodeLink.url)

            if epNo >= minEp && epNo <= maxEp {
                desiredEpisodeLinks = append(desiredEpisodeLinks, episodeLink.url)
                desiredEpisodeTitles = append(desiredEpisodeTitles,episodeLink.title)
            }
        }

        if len(desiredEpisodeLinks) == 0{
            return nil,errors.New("No episode found")
        }
        actualMinEp := episodeNo(desiredEpisodeLinks[0])
        if minEp > actualMinEp {
            actualMinEp = minEp
        }

        actualMaxEp := episodeNo(desiredEpisodeLinks[len(desiredEpisodeLinks)-1])
        if maxEp < actualMaxEp {
            actualMaxEp = maxEp
        }
        log.Printf("fetching image links for episodes %d through %d", actualMinEp, actualMaxEp)

        var episodeBatches []EpisodeBatch
        for start := 0; start < len(desiredEpisodeLinks); start += epsPerBatch {
            end := start + epsPerBatch
            if end > len(desiredEpisodeLinks) {
                end = len(desiredEpisodeLinks)
            }
            episodeBatches = append(episodeBatches, EpisodeBatch{
                imgLinks: getImgLinksForEpisodes(desiredEpisodeLinks[start:end], actualMaxEp),
                title:    createTitle(desiredEpisodeTitles[start:end]),
                minEp:    episodeNo(desiredEpisodeLinks[start]),
                maxEp:    episodeNo(desiredEpisodeLinks[end-1]),
            })
        }

        return episodeBatches, nil
    }
}

func createTitle(episodetitles []string) string{
    var title string

    for _, episodeTitle := range episodetitles {
        title = title+episodeTitle+"_"
    }
    last:=len(title)-1

    return title[:last]
}
func getAllEpisodeLinks(url string) []EpisodeInfo {
    re := regexp.MustCompile("&page=[0-9]+")
    episodeSet := make(map[EpisodeInfo]struct{})
//    episodeTitleSet := make(map[string]struct{})
    foundLastPage := false
    for page := 1; !foundLastPage; page++ {
        url = re.ReplaceAllString(url, "") + fmt.Sprintf("&page=%d", page)
        episodes, err := getEpisodeLinksForPage(url)

        if err != nil {
            break
        }
        for _, episode := range episodes {
            // when you go past the last page, it just rerenders the last page
            if _, ok := episodeSet[episode]; ok {
                foundLastPage = true
                break
            }
            episodeSet[episode] = struct{}{}
        }

        if !foundLastPage {
            log.Printf(url)
        }

    }

    allEpisode := make([]EpisodeInfo, 0, len(episodeSet))
    for episode := range episodeSet {
        allEpisode = append(allEpisode, episode)
    }
    // extract episode_no from url and sort by it
    sort.Slice(allEpisode, func(i, j int) bool {
        return episodeNo(allEpisode[i].url) < episodeNo(allEpisode[j].url)
    })
    return allEpisode
}

func episodeNo(episodeLink string) int {
//    log.Printf("%s",episodeLink)
    re := regexp.MustCompile("episode_no=([0-9]+)")
    matches := re.FindStringSubmatch(episodeLink)
    if len(matches) != 2 {
        log.Printf("episodeNo not found %d",len(matches))
        return 0
    }

    episodeNo, err := strconv.Atoi(matches[1])

    if err != nil {
        log.Printf("episodeNo %d",matches[1])
        return 0
    }
    return episodeNo
}

func getImgLinksForEpisodes(episodeLinks []string, actualMaxEp int) []string {
    var allImgLinks []string
    for _, episodeLink := range episodeLinks {
        log.Printf("fetching image links for episode %d/%d", episodeNo(episodeLink), actualMaxEp)
        allImgLinks = append(allImgLinks, getImgLinksForEpisode(episodeLink)...)
    }
    return allImgLinks
}

func fetchImage(imgLink string) []byte {
    req, e := http.NewRequest("GET", imgLink, nil)
    if e != nil {
        fmt.Println(e)
        os.Exit(1)
    }
    req.Header.Set("Referer", "http://www.webtoons.com")

    response, err := http.DefaultClient.Do(req)
    if err != nil {
        fmt.Println(err.Error())
        os.Exit(1)
    }
    defer func(Body io.ReadCloser) {
        err := Body.Close()
        if err != nil {
            fmt.Println(err.Error())
            os.Exit(1)
        }
    }(response.Body)

    buff := new(bytes.Buffer)
    _, err = buff.ReadFrom(response.Body)
    if err != nil {
        fmt.Println(err.Error())
        os.Exit(1)
    }
    return buff.Bytes()
}

func getComicFile(format string) ComicFile {
    var comic ComicFile
    var err error
    comic = newPDFComicFile()
    if format == "cbz" {
        comic, err = newCBZComicFile()
        if err != nil {
            fmt.Println(err.Error())
            os.Exit(1)
        }
    }
    return comic
}

type Opts struct {
    url        string
    minEp      int
    maxEp      int
    epsPerFile int
    format     string

}

func parseOpts(args []string) Opts {

    if len(args) < 2 {
        fmt.Println("Usage: webtoon-dl <url>")
        os.Exit(1)
    }

    database = flag.Bool("db", false, "Database mode get url from sqlfile for multiple webtoon")
    confOverride = flag.Bool("confOverride", false, "Override database config by parameter store in database")
    FileVerify = flag.Bool("file", false, "Check file if exist instead of recreate it dirrectly")

    NoLog = flag.Bool("NoLog", false, "print output")

    EpisodeGoroutine = flag.Int("E", 10, "Number of episode per webtoon download in the same time")
    WebtoonGoroutine = flag.Int("W", 3, "Numer of webtoon download in the same time")
    MaxWebtoonGoroutine= flag.Bool("MW", false, "Treat all webtoon at once")

    minEp := flag.Int("min-ep", 0, "Minimum episode number to download (inclusive)")
    maxEp := flag.Int("max-ep", math.MaxInt, "Maximum episode number to download (inclusive)")

    epsPerFile := flag.Int("eps-per-file", 1, "Number of episodes to put in each PDF file")
    format := flag.String("format", "pdf", "Output format (pdf or cbz)")

    url := flag.String("u", "" ,"URL")

    flag.Parse()

    if *minEp > *maxEp {
        fmt.Println("min-ep must be less than or equal to max-ep")
        os.Exit(1)
    }
    if *epsPerFile < 1 {
        fmt.Println("eps-per-file must be greater than or equal to 1")
        os.Exit(1)
    }
    if *minEp < 0 {
        fmt.Println("min-ep must be greater than or equal to 0")
        os.Exit(1)
    }

    return Opts{
        url:        *url,
        minEp:      *minEp,
        maxEp:      *maxEp,
        epsPerFile: *epsPerFile,
        format:     *format,
    }
}

func getOutFile(opts Opts, episodeBatch EpisodeBatch) string {
    outURL := strings.ReplaceAll(opts.url, "http://", "")
    outURL = strings.ReplaceAll(outURL, "https://", "")
    outURL = strings.ReplaceAll(outURL, "www.", "")
    outURL = strings.ReplaceAll(outURL, "webtoons.com/", "")
    outURL = strings.Split(outURL, "?")[0]
    outURL = strings.ReplaceAll(outURL, "/viewer", "")
    outURL = strings.ReplaceAll(outURL, "/", "-")
    if episodeBatch.minEp != episodeBatch.maxEp {
        outURL = fmt.Sprintf("%s-epNo%d-epNo%d.%s", outURL, episodeBatch.minEp, episodeBatch.maxEp, opts.format)
    } else {
        outURL = fmt.Sprintf("%s-epNo%d.%s", outURL, episodeBatch.minEp, opts.format)
    }

    return outURL

}

func getWebtoonTitle(opts Opts) (string,string,error) {

    _, err :=url.ParseRequestURI(opts.url)
    if err != nil {
            return "","",err

    }

    outURL := strings.ReplaceAll(opts.url, "http://", "")
    outURL = strings.ReplaceAll(outURL, "https://", "")
    outURL = strings.ReplaceAll(outURL, "www.", "")
    outURL = strings.ReplaceAll(outURL, "webtoons.com/", "")
    lang := strings.Split(outURL, "/")[0]
    title := strings.Split(outURL, "/")[2]
    return title, lang, nil
}




func saveBatch(pool *gopool.GoPool,title string, lang string, opts Opts, episodeBatch EpisodeBatch, totalEpisodes int)  {
    defer pool.Done()
    defer func() {
        if err := recover(); err != nil {
            log.Printf("Recovered: %v", err)
        }
    }()
    var err error

    outFile := fmt.Sprintf("webtoon/%s/%s/%s.%s", title, lang, episodeBatch.title, opts.format)

    _, fileExist := os.Stat(outFile);
    if (!*FileVerify ||  fileExist != nil){

        comicFile := getComicFile(opts.format)
            for idx, imgLink := range episodeBatch.imgLinks {
                if strings.Contains(imgLink, ".gif") {
                    fmt.Println(fmt.Sprintf("WARNING: skipping gif %s", imgLink))
                    continue
                }
                err := comicFile.addImage(fetchImage(imgLink))
                if err != nil {
                    println("********************")
                    panic(err.Error())
                }

                log.Printf(
                        "Title: %s saving episodes %d through %d of %d: added page %d/%d",
                        title,
                        episodeBatch.minEp,
                        episodeBatch.maxEp,
                        totalEpisodes,
                        idx+1,
                        len(episodeBatch.imgLinks),
                    )

            }
            err = comicFile.save(outFile)
            if err != nil {
                println("********************")
                panic(err.Error())
            }
            log.Printf("saved to %s", outFile)
    }
}

func GetWebtoon(db *sql.DB, opts Opts)(error){
    titre,lang,err := getWebtoonTitle (opts)

    if err != nil {
        panic(err)
    }

    outDirectory := fmt.Sprintf("webtoon/%s/%s/", titre, lang)
    os.MkdirAll(outDirectory,0755)



    episodeBatches,err := getEpisodeBatches(opts.url, opts.minEp, opts.maxEp, opts.epsPerFile)

    if err != nil {
        panic(err)
    }

    last_episode :=0

    totalPages := 0
    for _, episodeBatch := range episodeBatches {
        totalPages += len(episodeBatch.imgLinks)
    }
    totalEpisodes := episodeBatches[len(episodeBatches)-1].maxEp - episodeBatches[0].minEp + 1

    fmt.Println(fmt.Sprintf("Webtoon: %s lang: %s episode:%d totalPages:%d totalEpisodes:%d bathces:%d(ep %d)", titre, lang, opts.minEp,len(episodeBatches), opts.epsPerFile))

//    fmt.Println(fmt.Sprintf("found %d total image links across %d episodes", totalPages, totalEpisodes))
//    fmt.Println(fmt.Sprintf("saving into %d files with max of %d episodes per file", len(episodeBatches), opts.epsPerFile))

    pool := gopool.NewPool(*EpisodeGoroutine)

//    ctx, cancel := context.WithCancel(context.Background())
//    defer cancel() // Make sure it's called to release resources even if no errors

    for _, episodeBatch := range episodeBatches {
        pool.Add(1)
        go saveBatch(pool,titre, lang, opts , episodeBatch, totalEpisodes )
    }
    pool.Wait()
    last_episode=episodeBatches[len(episodeBatches)-1].maxEp

    request := fmt.Sprintf(
        "insert or replace into webtoon(titre,lang,url,last_chapter,epsPerFile,format) values ('%s','%s', '%s', %d,%d,'%s')",
        titre,
        lang,
        opts.url,
        last_episode,
        opts.epsPerFile,
        opts.format)
    log.Printf(request)

    _, err = db.Exec(request)
    if err != nil {
        panic(err)
    }

    fmt.Println(fmt.Sprintf("Completed Webtoon: %s lang: %s", titre, lang))

    return nil
}


func GetWebtoonBatch(pool *gopool.GoPool,db *sql.DB,opts Opts)(){
    defer pool.Done()
    defer func() {
        if err := recover(); err != nil {
            log.Printf("Recovered: %v", err)
        }
    }()
    GetWebtoon(db,opts)

}

func GetWebtoons(db *sql.DB, opts Opts)(){
    var sqlStmt string

    if(len(opts.url) == 0){
        sqlStmt = "SELECT url,last_chapter,epsPerFile,format FROM webtoon ";
    }else{
        sqlStmt = "SELECT url,last_chapter,epsPerFile,format FROM webtoon where url='" +opts.url+"'";
    }

    rows, err := db.Query(sqlStmt)
    if err != nil {
        log.Printf("ERROR %q: %s\n", err, sqlStmt)

    }else{
        var webtoons []Opts
        var url string
        var format string
        var epsPerFile int
        var last_chapter int
        defer rows.Close()

        for rows.Next() {
            err = rows.Scan( &url, &last_chapter,&epsPerFile,&format)
            if err != nil {
                println(url)
                log.Fatal(err) //*
            }
            opts.url = url

            //by default download until the end
            if !*confOverride {
                opts.epsPerFile=epsPerFile
                opts.format=format
                opts.minEp=last_chapter
            }
            webtoons = append(webtoons, opts)
        }

        if *MaxWebtoonGoroutine {
            *WebtoonGoroutine = len(webtoons)
        }
        pool := gopool.NewPool(*WebtoonGoroutine)

        for _, opts := range webtoons {
            defer pool.Done()
            pool.Add(1)
            go GetWebtoonBatch(pool,db,opts)

        }
        pool.Wait()
        //bug did not exit function when finish
        os.Exit(0)
    }
}

//open database create table if did not exist
func openDatabse(file string)(*sql.DB){
    db, err := sql.Open("sqlite3", file)

    if err != nil {
        println("DB erreur")
        log.Fatal(err) //*
    }

    sqlStmt := "SELECT name FROM sqlite_master WHERE type='table' AND name='webtoon'";

    rows, err := db.Query(sqlStmt)
    if err != nil {
        println("ERROR %q: %s\n", err, sqlStmt)
        log.Fatal(err) //*
    }
    NotExist:= true

    for rows.Next() {
        var name string
        err = rows.Scan( &name)
        if err != nil {
            println(err)
            log.Fatal(err) //*
        }
        NotExist = false
    }

    if NotExist {
        log.Printf("create table")
        sqlStmt := "create table webtoon (titre text, lang text,url,text,last_chapter integer,epsPerFile integer,format text, PRIMARY KEY(titre,lang));"

        _, err := db.Exec(sqlStmt)
        if err != nil {
            println("ERROR %q: %s\n", err, sqlStmt)
            log.Fatal(err) //*
        }
    }
    return (db)
}

func main() {
    logFile, err := os.OpenFile("log", os.O_RDWR | os.O_CREATE, 0666)
    if err != nil {
        log.Fatalf("error opening file: %v", err)
    }
    defer logFile.Close()

    opts := parseOpts(os.Args)

    if !*NoLog {
       log.SetOutput(logFile)
    }

    if *FileVerify {
        println("file verify on")
    }

    db:=openDatabse("./database.db")
    defer db.Close()

    if *database {
        GetWebtoons(db,opts)


    }else{
        GetWebtoon(db,opts)
    }
}
