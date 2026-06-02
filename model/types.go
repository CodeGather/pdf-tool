package model

import "encoding/json"

// -------- 顶层结构 --------

// ReplaceConfig 是 JSON 文件的顶层结构
type ReplaceConfig struct {
	ShopName  string         `json:"shopName"`
	TableConf []TableColumn  `json:"table-config"`
	BrandConf BrandConfig    `json:"brand-config"`
	ExcelData ExcelData      `json:"excel-data"`
	DbData    DbData         `json:"db-data"`
	FileData  FileData       `json:"file-data"`
}

// -------- Table --------

type TableColumn struct {
	Label string   `json:"label"`
	Key   string   `json:"key"`
	Width *float64 `json:"width,omitempty"` // nil 表示无固定宽度
	Align string   `json:"align,omitempty"`
}

// -------- Brand Config --------

// BrandConfig isNew 表格和边框样式配置
type BrandConfig struct {
	// 顶层控制字段
	AddCatalogue    bool         `json:"addCatalogue,omitempty"`
	AddGuide        bool         `json:"addGuide,omitempty"`
	AddInstallDir   bool         `json:"addInstallDir,omitempty"`
	AddPageNumber   bool         `json:"addPageNumber,omitempty"`
	AddPageNumbers  []int        `json:"addPageNumbers,omitempty"`
	AddShopName     bool         `json:"addShopName,omitempty"`
	BrandName       string       `json:"brandName,omitempty"`
	Catalogue       *CatCfg      `json:"catalogue,omitempty"`
	CustomTexts     []string     `json:"customTexts,omitempty"`
	Guide           *GuideCfg    `json:"guide,omitempty"`
	Header          []string     `json:"header,omitempty"`
	ID              int          `json:"id,omitempty"`
	LastPageNumber  bool         `json:"lastPageNumber,omitempty"`
	PageNumber      *PageNumCfg  `json:"pageNumber,omitempty"`
	ShopName        *ShopNameCfg `json:"shopName,omitempty"`
	SplitSeparator  []string     `json:"splitSeparator,omitempty"`
}

// GuideCfg 引导线/辅助绘制的样式配置
type GuideCfg struct {
	BorderColor  Color   `json:"borderColor"`
	BorderSize   float64 `json:"borderSize"`
	Color        Color   `json:"color"`
	DescColor    Color   `json:"descColor"`
	DescFontSize float64 `json:"descFontSize"`
	FontData     string  `json:"fontData,omitempty"`
	FontFamily   string  `json:"fontFamily,omitempty"`
	FontSize     float64 `json:"fontSize"`
}

// CatCfg 目录配置
type CatCfg struct {
	Background   string  `json:"background,omitempty"`
	Color        Color   `json:"color"`
	FontData     string  `json:"fontData,omitempty"`
	FontFamily   string  `json:"fontFamily,omitempty"`
	IndentSize   float64 `json:"indentSize"`
	Left         float64 `json:"left"`
	LineHeight   float64 `json:"lineHeight"`
	OneFontSize  float64 `json:"oneFontSize"`
	TitleFontSize float64 `json:"titleFontSize"`
	Top          float64 `json:"top"`
	TwoFontSize  float64 `json:"twoFontSize"`
}

// PageNumCfg 页码配置
type PageNumCfg struct {
	Bottom   float64 `json:"bottom"`
	Color    Color   `json:"color"`
	FontData string  `json:"fontData,omitempty"`
	FontFamily string `json:"fontFamily,omitempty"`
	FontSize float64 `json:"fontSize"`
	Left     float64 `json:"left"`
}

// ShopNameCfg 店铺名称配置
type ShopNameCfg struct {
	Background Color   `json:"background"`
	Bottom     float64 `json:"bottom"`
	Color      Color   `json:"color"`
	FontFamily string  `json:"fontFamily,omitempty"`
	FontSize   float64 `json:"fontSize"`
	Left       float64 `json:"left"`
	Width      float64 `json:"width"`
}

type Color struct {
	Red     float64 `json:"red"`
	Green   float64 `json:"green"`
	Blue    float64 `json:"blue"`
	Opacity float64 `json:"opacity"`
	RGB     string  `json:"rgb,omitempty"`
}

// -------- Excel Data --------

type ExcelData map[string]LampItem

type LampItem struct {
	CounterCode string  `json:"柜台编号"`
	Channel     string  `json:"渠道"`
	Region      string  `json:"区域"`
	City        string  `json:"城市"`
	ShopName    string  `json:"柜台名称"`
	LampNo      string  `json:"灯位编号"`
	LampNote    string  `json:"灯位备注"`
	Category    string  `json:"大类"`
	Position    string  `json:"灯位位置"`
	Material    string  `json:"材质"`
	VisibleW    float64 `json:"可见宽"`
	VisibleH    float64 `json:"可见长"`
	LaunchFilm  string  `json:"上市灯片,omitempty"`
	RatioWH     float64 `json:"比例(W/H)"`
	RatioHW     float64 `json:"比例(H/W)"`
	Direction   string  `json:"冲外/冲内,omitempty"`
	Content     string  `json:"画面内容"`
	ContentPos  string  `json:"画面位置,omitempty"`
	IsNew       *bool   `json:"isNew,omitempty"`
	LaunchNote  string  `json:"上市备注,omitempty"`
	Remark      string  `json:"备注,omitempty"`
}

func (l *LampItem) IsNewValue() bool {
	if l.IsNew != nil {
		return *l.IsNew
	}
	if l.LaunchFilm == "Y" || l.LaunchFilm == "y" {
		return true
	}
	return false
}

// -------- DB Data --------

type DbData struct {
	ID       int      `json:"id"`
	Brand    string   `json:"品牌"`
	ShopName string   `json:"店铺名称"`
	Supplier string   `json:"柜台供应商"`
	Installer string  `json:"安装供应商"`
	Form     string   `json:"柜台形式"`
	IsOpen   string   `json:"是否开业"`
	Lamps    []DbLamp `json:"lamp"`
}

type DbLamp struct {
	ID           int             `json:"id"`
	Brand        string          `json:"品牌"`
	ShopName     string          `json:"店铺名称"`
	Width        string          `json:"宽度"`
	Height       string          `json:"高度"`
	Scale        string          `json:"缩放"`
	File         string          `json:"文件"`
	PDFTitle     string          `json:"PDF标题"`
	NumListRaw   string          `json:"编号列表"`  // JSON 字符串
	NumList      []ImageNumEntry `json:"-"`       // 反序列化后的结构
}

// UnmarshalJSON 自定义解码：将编号列表 JSON 字符串反序列化
func (l *DbLamp) UnmarshalJSON(data []byte) error {
	type Alias DbLamp
	aux := &struct{ *Alias }{Alias: (*Alias)(l)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if l.NumListRaw != "" {
		if err := json.Unmarshal([]byte(l.NumListRaw), &l.NumList); err != nil {
			return err
		}
	}
	return nil
}

type ImageNumEntry struct {
	Distance float64   `json:"distance"`
	HasMatch bool      `json:"hasMatch"`
	ImageID  string    `json:"imageId"`
	Image    ImageInfo `json:"image"`
	Num      NumInfo   `json:"num"`
	Nums     []NumInfo `json:"nums"`
}

type ImageInfo struct {
	DataURL           *string `json:"dataUrl,omitempty"`
	DisHeight         float64 `json:"disHeight"`
	DisWidth          float64 `json:"disWidth"`
	Height            float64 `json:"height"`
	ID                string  `json:"id"`
	Index             int     `json:"index"`
	OriginalTransform struct {
		Height float64 `json:"height"`
		Width  float64 `json:"width"`
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
	} `json:"originalTransform"`
	PageHeight float64 `json:"pageHeight"`
	PageWidth  float64 `json:"pageWidth"`
	Rotate     float64 `json:"rotate"`
	Width      float64 `json:"width"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
}

type NumInfo struct {
	Distance float64 `json:"distance"`
	IsInside bool    `json:"isInside"`
	Str      string  `json:"str"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
}

// -------- File Data --------

type FileData map[string]FileEntry

type FileEntry struct {
	Path       string      `json:"path"`
	TotalPages *int        `json:"totalPages,omitempty"`
	Lazy       bool        `json:"lazy"`
	Pages      []PageEntry `json:"pages"`
}

type PageEntry struct {
	Title          string       `json:"title"`
	CatalogueTitle string       `json:"catalogueTitle"`
	P              int          `json:"p"`
	Items          []interface{} `json:"items"`
	Images         []ImageMeta  `json:"images"`
	Width          float64      `json:"width"`
	Height         float64      `json:"height"`
	Path           string       `json:"path"`
}

type ImageMeta struct {
	Ext    string  `json:"ext"`
	Height float64 `json:"height"`
	Index  int     `json:"index,omitempty"`
	Object int     `json:"object,omitempty"`
	Page   int     `json:"page"`
	Path   string  `json:"path"`
	Source string  `json:"source"`
	Time   string  `json:"time"`
	Type   string  `json:"type"`
	Width  float64 `json:"width"`
}