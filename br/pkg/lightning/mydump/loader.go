// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mydump

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/br/pkg/lightning/common"
	"github.com/pingcap/tidb/br/pkg/lightning/config"
	"github.com/pingcap/tidb/br/pkg/lightning/log"
	"github.com/pingcap/tidb/br/pkg/storage"
	regexprrouter "github.com/pingcap/tidb/util/regexpr-router"
	filter "github.com/pingcap/tidb/util/table-filter"
	"go.uber.org/zap"
)

// MDDatabaseMeta contains some parsed metadata for a database in the source by MyDumper Loader.
type MDDatabaseMeta struct {
	Name       string
	SchemaFile FileInfo
	Tables     []*MDTableMeta
	Views      []*MDTableMeta
	charSet    string
}

// NewMDDatabaseMeta creates an Mydumper database meta with specified character set.
func NewMDDatabaseMeta(charSet string) *MDDatabaseMeta {
	return &MDDatabaseMeta{
		charSet: charSet,
	}
}

// GetSchema gets the schema SQL for a source database.
func (m *MDDatabaseMeta) GetSchema(ctx context.Context, store storage.ExternalStorage) string {
	if m.SchemaFile.FileMeta.Path != "" {
		schema, err := ExportStatement(ctx, store, m.SchemaFile, m.charSet)
		if err != nil {
			log.FromContext(ctx).Warn("failed to extract table schema",
				zap.String("Path", m.SchemaFile.FileMeta.Path),
				log.ShortError(err),
			)
		} else if schemaStr := strings.TrimSpace(string(schema)); schemaStr != "" {
			return schemaStr
		}
	}
	// set default if schema sql is empty or failed to extract.
	return "CREATE DATABASE IF NOT EXISTS " + common.EscapeIdentifier(m.Name)
}

// MDTableMeta contains some parsed metadata for a table in the source by MyDumper Loader.
type MDTableMeta struct {
	DB           string
	Name         string
	SchemaFile   FileInfo
	DataFiles    []FileInfo
	charSet      string
	TotalSize    int64
	IndexRatio   float64
	IsRowOrdered bool
}

// SourceFileMeta contains some analyzed metadata for a source file by MyDumper Loader.
type SourceFileMeta struct {
	Path        string
	Type        SourceType
	Compression Compression
	SortKey     string
	FileSize    int64
}

// NewMDTableMeta creates an Mydumper table meta with specified character set.
func NewMDTableMeta(charSet string) *MDTableMeta {
	return &MDTableMeta{
		charSet: charSet,
	}
}

// GetSchema gets the table-creating SQL for a source table.
func (m *MDTableMeta) GetSchema(ctx context.Context, store storage.ExternalStorage) (string, error) {
	schemaFilePath := m.SchemaFile.FileMeta.Path
	if len(schemaFilePath) <= 0 {
		return "", errors.Errorf("schema file is missing for the table '%s.%s'", m.DB, m.Name)
	}
	fileExists, err := store.FileExists(ctx, schemaFilePath)
	if err != nil {
		return "", errors.Annotate(err, "check table schema file exists error")
	}
	if !fileExists {
		return "", errors.Errorf("the provided schema file (%s) for the table '%s.%s' doesn't exist", schemaFilePath, m.DB, m.Name)
	}
	schema, err := ExportStatement(ctx, store, m.SchemaFile, m.charSet)
	if err != nil {
		log.FromContext(ctx).Error("failed to extract table schema",
			zap.String("Path", m.SchemaFile.FileMeta.Path),
			log.ShortError(err),
		)
		return "", err
	}
	return string(schema), nil
}

// MDLoaderSetupConfig stores the configs when setting up a MDLoader.
// This can control the behavior when constructing an MDLoader.
type MDLoaderSetupConfig struct {
	// MaxScanFiles specifies the maximum number of files to scan.
	// If the value is <= 0, it means the number of data source files will be scanned as many as possible.
	MaxScanFiles int
}

// DefaultMDLoaderSetupConfig generates a default MDLoaderSetupConfig.
func DefaultMDLoaderSetupConfig() *MDLoaderSetupConfig {
	return &MDLoaderSetupConfig{
		MaxScanFiles: 0, // By default, the loader will scan all the files.
	}
}

// MDLoaderSetupOption is the option type for setting up a MDLoaderSetupConfig.
type MDLoaderSetupOption func(cfg *MDLoaderSetupConfig)

// WithMaxScanFiles generates an option that limits the max scan files when setting up a MDLoader.
func WithMaxScanFiles(maxScanFiles int) MDLoaderSetupOption {
	return func(cfg *MDLoaderSetupConfig) {
		cfg.MaxScanFiles = maxScanFiles
	}
}

// MDLoader is for 'Mydumper File Loader', which loads the files in the data source and generates a set of metadata.
type MDLoader struct {
	store  storage.ExternalStorage
	dbs    []*MDDatabaseMeta
	filter filter.Filter
	// router     *router.Table
	router     *regexprrouter.RouteTable
	fileRouter FileRouter
	charSet    string
}

type mdLoaderSetup struct {
	loader        *MDLoader
	dbSchemas     []FileInfo
	tableSchemas  []FileInfo
	viewSchemas   []FileInfo
	tableDatas    []FileInfo
	dbIndexMap    map[string]int
	tableIndexMap map[filter.Table]int
	setupCfg      *MDLoaderSetupConfig
}

// NewMyDumpLoader constructs a MyDumper loader that scanns the data source and constructs a set of metadatas.
func NewMyDumpLoader(ctx context.Context, cfg *config.Config, opts ...MDLoaderSetupOption) (*MDLoader, error) {
	u, err := storage.ParseBackend(cfg.Mydumper.SourceDir, nil)
	if err != nil {
		return nil, common.NormalizeError(err)
	}
	s, err := storage.New(ctx, u, &storage.ExternalStorageOptions{})
	if err != nil {
		return nil, common.NormalizeError(err)
	}

	return NewMyDumpLoaderWithStore(ctx, cfg, s, opts...)
}

// NewMyDumpLoaderWithStore constructs a MyDumper loader with the provided external storage that scanns the data source and constructs a set of metadatas.
func NewMyDumpLoaderWithStore(ctx context.Context, cfg *config.Config, store storage.ExternalStorage, opts ...MDLoaderSetupOption) (*MDLoader, error) {
	var r *regexprrouter.RouteTable
	var err error

	mdLoaderSetupCfg := DefaultMDLoaderSetupConfig()
	for _, o := range opts {
		o(mdLoaderSetupCfg)
	}

	if len(cfg.Routes) > 0 && len(cfg.Mydumper.FileRouters) > 0 {
		return nil, common.ErrInvalidConfig.GenWithStack("table route is deprecated, can't config both [routes] and [mydumper.files]")
	}

	if len(cfg.Routes) > 0 {
		r, err = regexprrouter.NewRegExprRouter(cfg.Mydumper.CaseSensitive, cfg.Routes)
		if err != nil {
			return nil, common.ErrInvalidConfig.Wrap(err).GenWithStack("invalid table route rule")
		}
	}

	// use the legacy black-white-list if defined. otherwise use the new filter.
	var f filter.Filter
	if cfg.HasLegacyBlackWhiteList() {
		f, err = filter.ParseMySQLReplicationRules(&cfg.BWList)
	} else {
		f, err = filter.Parse(cfg.Mydumper.Filter)
	}
	if err != nil {
		return nil, common.ErrInvalidConfig.Wrap(err).GenWithStack("parse filter failed")
	}
	if !cfg.Mydumper.CaseSensitive {
		f = filter.CaseInsensitive(f)
	}

	fileRouteRules := cfg.Mydumper.FileRouters
	if cfg.Mydumper.DefaultFileRules {
		fileRouteRules = append(fileRouteRules, defaultFileRouteRules...)
	}

	fileRouter, err := NewFileRouter(fileRouteRules, log.FromContext(ctx))
	if err != nil {
		return nil, common.ErrInvalidConfig.Wrap(err).GenWithStack("parse file routing rule failed")
	}

	mdl := &MDLoader{
		store:      store,
		filter:     f,
		router:     r,
		charSet:    cfg.Mydumper.CharacterSet,
		fileRouter: fileRouter,
	}

	setup := mdLoaderSetup{
		loader:        mdl,
		dbIndexMap:    make(map[string]int),
		tableIndexMap: make(map[filter.Table]int),
		setupCfg:      mdLoaderSetupCfg,
	}

	if err := setup.setup(ctx, mdl.store); err != nil {
		if errors.ErrorEqual(err, common.ErrTooManySourceFiles) {
			return mdl, err
		}
		return nil, errors.Trace(err)
	}

	return mdl, nil
}

type fileType int

const (
	fileTypeDatabaseSchema fileType = iota
	fileTypeTableSchema
	fileTypeTableData
)

// String implements the Stringer interface.
func (ftype fileType) String() string {
	switch ftype {
	case fileTypeDatabaseSchema:
		return "database schema"
	case fileTypeTableSchema:
		return "table schema"
	case fileTypeTableData:
		return "table data"
	default:
		return "(unknown)"
	}
}

// FileInfo contains the information for a data file in a table.
type FileInfo struct {
	TableName filter.Table
	FileMeta  SourceFileMeta
}

// setup the `s.loader.dbs` slice by scanning all *.sql files inside `dir`.
//
// The database and tables are inserted in a consistent order, so creating an
// MDLoader twice with the same data source is going to produce the same array,
// even after killing Lightning.
//
// This is achieved by using `filepath.Walk` internally which guarantees the
// files are visited in lexicographical order (note that this does not mean the
// databases and tables in the end are ordered lexicographically since they may
// be stored in different subdirectories).
//
// Will sort tables by table size, this means that the big table is imported
// at the latest, which to avoid large table take a long time to import and block
// small table to release index worker.
func (s *mdLoaderSetup) setup(ctx context.Context, store storage.ExternalStorage) error {
	/*
		Mydumper file names format
			db    —— {db}-schema-create.sql
			table —— {db}.{table}-schema.sql
			sql   —— {db}.{table}.{part}.sql / {db}.{table}.sql
	*/
	var gerr error
	if err := s.listFiles(ctx, store); err != nil {
		if errors.ErrorEqual(err, common.ErrTooManySourceFiles) {
			gerr = err
		} else {
			return common.ErrStorageUnknown.Wrap(err).GenWithStack("list file failed")
		}
	}
	if err := s.route(); err != nil {
		return common.ErrTableRoute.Wrap(err).GenWithStackByArgs()
	}

	// setup database schema
	if len(s.dbSchemas) != 0 {
		for _, fileInfo := range s.dbSchemas {
			if _, dbExists := s.insertDB(fileInfo); dbExists && s.loader.router == nil {
				return common.ErrInvalidSchemaFile.GenWithStack("invalid database schema file, duplicated item - %s", fileInfo.FileMeta.Path)
			}
		}
	}

	if len(s.tableSchemas) != 0 {
		// setup table schema
		for _, fileInfo := range s.tableSchemas {
			if _, _, tableExists := s.insertTable(fileInfo); tableExists && s.loader.router == nil {
				return common.ErrInvalidSchemaFile.GenWithStack("invalid table schema file, duplicated item - %s", fileInfo.FileMeta.Path)
			}
		}
	}

	if len(s.viewSchemas) != 0 {
		// setup view schema
		for _, fileInfo := range s.viewSchemas {
			_, tableExists := s.insertView(fileInfo)
			if !tableExists {
				// we are not expect the user only has view schema without table schema when user use dumpling to get view.
				// remove the last `-view.sql` from path as the relate table schema file path
				return common.ErrInvalidSchemaFile.GenWithStack("invalid view schema file, miss host table schema for view '%s'", fileInfo.TableName.Name)
			}
		}
	}

	// Sql file for restore data
	for _, fileInfo := range s.tableDatas {
		// set a dummy `FileInfo` here without file meta because we needn't restore the table schema
		tableMeta, _, _ := s.insertTable(FileInfo{TableName: fileInfo.TableName})
		tableMeta.DataFiles = append(tableMeta.DataFiles, fileInfo)
		tableMeta.TotalSize += fileInfo.FileMeta.FileSize
	}

	for _, dbMeta := range s.loader.dbs {
		// Put the small table in the front of the slice which can avoid large table
		// take a long time to import and block small table to release index worker.
		meta := dbMeta
		sort.SliceStable(meta.Tables, func(i, j int) bool {
			return meta.Tables[i].TotalSize < meta.Tables[j].TotalSize
		})

		// sort each table source files by sort-key
		for _, tbMeta := range meta.Tables {
			dataFiles := tbMeta.DataFiles
			sort.SliceStable(dataFiles, func(i, j int) bool {
				return dataFiles[i].FileMeta.SortKey < dataFiles[j].FileMeta.SortKey
			})
		}
	}

	return gerr
}

func (s *mdLoaderSetup) listFiles(ctx context.Context, store storage.ExternalStorage) error {
	// `filepath.Walk` yields the paths in a deterministic (lexicographical) order,
	// meaning the file and chunk orders will be the same everytime it is called
	// (as long as the source is immutable).
	totalScannedFileCount := 0
	err := store.WalkDir(ctx, &storage.WalkOption{}, func(path string, size int64) error {
		logger := log.FromContext(ctx).With(zap.String("path", path))
		totalScannedFileCount++
		if s.setupCfg.MaxScanFiles > 0 && totalScannedFileCount > s.setupCfg.MaxScanFiles {
			return common.ErrTooManySourceFiles
		}
		res, err := s.loader.fileRouter.Route(filepath.ToSlash(path))
		if err != nil {
			return errors.Annotatef(err, "apply file routing on file '%s' failed", path)
		}
		if res == nil {
			logger.Info("[loader] file is filtered by file router")
			return nil
		}

		info := FileInfo{
			TableName: filter.Table{Schema: res.Schema, Name: res.Name},
			FileMeta:  SourceFileMeta{Path: path, Type: res.Type, Compression: res.Compression, SortKey: res.Key, FileSize: size},
		}

		if s.loader.shouldSkip(&info.TableName) {
			logger.Debug("[filter] ignoring table file")

			return nil
		}

		switch res.Type {
		case SourceTypeSchemaSchema:
			s.dbSchemas = append(s.dbSchemas, info)
		case SourceTypeTableSchema:
			s.tableSchemas = append(s.tableSchemas, info)
		case SourceTypeViewSchema:
			s.viewSchemas = append(s.viewSchemas, info)
		case SourceTypeSQL, SourceTypeCSV, SourceTypeParquet:
			s.tableDatas = append(s.tableDatas, info)
		}

		logger.Debug("file route result", zap.String("schema", res.Schema),
			zap.String("table", res.Name), zap.Stringer("type", res.Type))

		return nil
	})

	return errors.Trace(err)
}

func (l *MDLoader) shouldSkip(table *filter.Table) bool {
	if len(table.Name) == 0 {
		return !l.filter.MatchSchema(table.Schema)
	}
	return !l.filter.MatchTable(table.Schema, table.Name)
}

func (s *mdLoaderSetup) route() error {
	r := s.loader.router
	if r == nil {
		return nil
	}

	type dbInfo struct {
		fileMeta SourceFileMeta
		count    int // means file count(db/table/view schema and table data)
	}

	knownDBNames := make(map[string]*dbInfo)
	for _, info := range s.dbSchemas {
		knownDBNames[info.TableName.Schema] = &dbInfo{
			fileMeta: info.FileMeta,
			count:    1,
		}
	}
	for _, info := range s.tableSchemas {
		knownDBNames[info.TableName.Schema].count++
	}
	for _, info := range s.viewSchemas {
		knownDBNames[info.TableName.Schema].count++
	}
	for _, info := range s.tableDatas {
		knownDBNames[info.TableName.Schema].count++
	}

	runRoute := func(arr []FileInfo) error {
		for i, info := range arr {
			rawDB, rawTable := info.TableName.Schema, info.TableName.Name
			targetDB, targetTable, err := r.Route(rawDB, rawTable)
			if err != nil {
				return errors.Trace(err)
			}
			if targetDB != rawDB {
				oldInfo := knownDBNames[rawDB]
				oldInfo.count--
				newInfo, ok := knownDBNames[targetDB]
				if !ok {
					newInfo = &dbInfo{fileMeta: oldInfo.fileMeta, count: 1}
					s.dbSchemas = append(s.dbSchemas, FileInfo{
						TableName: filter.Table{Schema: targetDB},
						FileMeta:  oldInfo.fileMeta,
					})
				}
				newInfo.count++
				knownDBNames[targetDB] = newInfo
			}
			arr[i].TableName = filter.Table{Schema: targetDB, Name: targetTable}
		}
		return nil
	}

	// route for schema table and view
	if err := runRoute(s.dbSchemas); err != nil {
		return errors.Trace(err)
	}
	if err := runRoute(s.tableSchemas); err != nil {
		return errors.Trace(err)
	}
	if err := runRoute(s.viewSchemas); err != nil {
		return errors.Trace(err)
	}
	if err := runRoute(s.tableDatas); err != nil {
		return errors.Trace(err)
	}
	// remove all schemas which has been entirely routed away(file count > 0)
	// https://github.com/golang/go/wiki/SliceTricks#filtering-without-allocating
	remainingSchemas := s.dbSchemas[:0]
	for _, info := range s.dbSchemas {
		if dbInfo := knownDBNames[info.TableName.Schema]; dbInfo.count > 0 {
			remainingSchemas = append(remainingSchemas, info)
		} else if dbInfo.count < 0 {
			// this should not happen if there are no bugs in the code
			return common.ErrTableRoute.GenWithStack("something wrong happened when route %s", info.TableName.String())
		}
	}
	s.dbSchemas = remainingSchemas
	return nil
}

func (s *mdLoaderSetup) insertDB(f FileInfo) (*MDDatabaseMeta, bool) {
	dbIndex, ok := s.dbIndexMap[f.TableName.Schema]
	if ok {
		return s.loader.dbs[dbIndex], true
	}
	s.dbIndexMap[f.TableName.Schema] = len(s.loader.dbs)
	ptr := &MDDatabaseMeta{
		Name:       f.TableName.Schema,
		SchemaFile: f,
		charSet:    s.loader.charSet,
	}
	s.loader.dbs = append(s.loader.dbs, ptr)
	return ptr, false
}

func (s *mdLoaderSetup) insertTable(fileInfo FileInfo) (tblMeta *MDTableMeta, dbExists bool, tableExists bool) {
	dbFileInfo := FileInfo{
		TableName: filter.Table{
			Schema: fileInfo.TableName.Schema,
		},
		FileMeta: SourceFileMeta{Type: SourceTypeSchemaSchema},
	}
	dbMeta, dbExists := s.insertDB(dbFileInfo)
	tableIndex, ok := s.tableIndexMap[fileInfo.TableName]
	if ok {
		return dbMeta.Tables[tableIndex], dbExists, true
	}
	s.tableIndexMap[fileInfo.TableName] = len(dbMeta.Tables)
	ptr := &MDTableMeta{
		DB:           fileInfo.TableName.Schema,
		Name:         fileInfo.TableName.Name,
		SchemaFile:   fileInfo,
		DataFiles:    make([]FileInfo, 0, 16),
		charSet:      s.loader.charSet,
		IndexRatio:   0.0,
		IsRowOrdered: true,
	}
	dbMeta.Tables = append(dbMeta.Tables, ptr)
	return ptr, dbExists, false
}

func (s *mdLoaderSetup) insertView(fileInfo FileInfo) (dbExists bool, tableExists bool) {
	dbFileInfo := FileInfo{
		TableName: filter.Table{
			Schema: fileInfo.TableName.Schema,
		},
		FileMeta: SourceFileMeta{Type: SourceTypeSchemaSchema},
	}
	dbMeta, dbExists := s.insertDB(dbFileInfo)
	_, ok := s.tableIndexMap[fileInfo.TableName]
	if ok {
		meta := &MDTableMeta{
			DB:           fileInfo.TableName.Schema,
			Name:         fileInfo.TableName.Name,
			SchemaFile:   fileInfo,
			charSet:      s.loader.charSet,
			IndexRatio:   0.0,
			IsRowOrdered: true,
		}
		dbMeta.Views = append(dbMeta.Views, meta)
	}
	return dbExists, ok
}

// GetDatabases gets the list of scanned MDDatabaseMeta for the loader.
func (l *MDLoader) GetDatabases() []*MDDatabaseMeta {
	return l.dbs
}

// GetStore gets the external storage used by the loader.
func (l *MDLoader) GetStore() storage.ExternalStorage {
	return l.store
}
