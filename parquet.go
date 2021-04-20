// Package parquet is not intended to be used as a general library.  The
// code generated by the 'parquetgen' command is what actually uses it for
// reading and writing parquet files.
package parquet

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"strings"

	"github.com/apache/thrift/lib/go/thrift"
	sch "github.com/viant/parquet/schema"
)

// Field holds the type information for a parquet column
type Field struct {
	Name           string
	Path           []string
	Types          []int
	Type           FieldFunc
	RepetitionType FieldFunc
	ConvertedType  *sch.ConvertedType
	LogicalType    *sch.LogicalType
}

// Page keeps track of metadata for each ColumnChunk
type Page struct {
	// N is the number of values in the ColumnChunk
	N      int
	Size   int
	Offset int64
	Codec  sch.CompressionCodec
}

type schema struct {
	fields []Field
	lookup map[string]sch.SchemaElement
}

func (s schema) schema() (int64, []*sch.SchemaElement) {
	out := make([]*sch.SchemaElement, 0, len(s.fields)+1)
	out = append(out, &sch.SchemaElement{
		Name: "root",
	})

	var children int32
	var z int32
	m := map[string]*sch.SchemaElement{}
	for _, f := range s.fields {
		if len(f.Path) > 1 {
			for i, name := range f.Path[:len(f.Path)-1] {
				par, ok := m[name]
				if !ok {
					children++
					parts := strings.Split(name, ".")
					rt := sch.FieldRepetitionType(f.Types[i])
					par = &sch.SchemaElement{
						Name:           parts[len(parts)-1],
						RepetitionType: &rt,
						NumChildren:    &z,
					}
					out = append(out, par)
				}
				n := *par.NumChildren
				n++
				par.NumChildren = &n
				m[name] = par
			}
		} else if len(f.Path) == 1 {
			children++
		}

		se := &sch.SchemaElement{
			Name:       f.Path[len(f.Path)-1],
			TypeLength: &z,
			Scale:      &z,
			Precision:  &z,
			FieldID:    &z,
		}

		f.Type(se)
		f.RepetitionType(se)
		out = append(out, se)
	}

	out[0].NumChildren = &children
	return int64(len(s.fields)), out
}

// Metadata keeps track of the things that need to
// be kept track of in order to write the FileMetaData
// at the end of the parquet file.
type Metadata struct {
	ts           *thrift.TSerializer
	schema       schema
	docs         int64
	pageDocs     int64
	rowGroupDocs int64
	rowGroups    []RowGroup
	metadata     *sch.FileMetaData
}

// Stats is passed in by each column's call to DoWrite
type Stats interface {
	NullCount() *int64
	DistinctCount() *int64
	Min() []byte
	Max() []byte
}

// New returns a Metadata struct and reads the first row group
// into memory.
func New(fields ...Field) *Metadata {
	ts := thrift.NewTSerializer()
	ts.Protocol = thrift.NewTCompactProtocolFactory().GetProtocol(ts.Transport)
	m := &Metadata{
		ts:     ts,
		schema: schemaElements(fields),
	}

	m.StartRowGroup(fields...)
	return m
}

// StartRowGroup is called when starting a new row group
func (m *Metadata) StartRowGroup(fields ...Field) {
	m.rowGroupDocs = 0
	m.rowGroups = append(m.rowGroups, RowGroup{
		fields:  schemaElements(fields),
		columns: make(map[string]sch.ColumnChunk),
	})
}

// NextDoc keeps track of how many documents have been
// added to this parquet file.  The final value of m.docs
// is used for the FileMetaData.NumRows
func (m *Metadata) NextDoc() {
	m.docs++
	m.rowGroupDocs++
	m.pageDocs++
}

// RowGroups returns a summary of each schema.RowGroup
func (m *Metadata) RowGroups() []RowGroup {
	rgs := make([]RowGroup, len(m.metadata.RowGroups))
	for i, rg := range m.metadata.RowGroups {
		rgs[i] = RowGroup{
			rowGroup: *rg,
			Rows:     rg.NumRows,
		}
	}
	return rgs
}

// WritePageHeader is called in order to finish writing to a column chunk.
func (m *Metadata) WritePageHeader(w io.Writer, pth []string, dataLen, compressedLen, defCount, count int, defLen, repLen int64, comp sch.CompressionCodec, stats Stats) error {
	ph := &sch.PageHeader{
		Type:                 sch.PageType_DATA_PAGE,
		UncompressedPageSize: int32(dataLen),
		CompressedPageSize:   int32(compressedLen),
		DataPageHeader: &sch.DataPageHeader{
			NumValues:               int32(count),
			Encoding:                sch.Encoding_PLAIN,
			DefinitionLevelEncoding: sch.Encoding_RLE,
			RepetitionLevelEncoding: sch.Encoding_RLE,
			Statistics: &sch.Statistics{
				NullCount:     stats.NullCount(),
				DistinctCount: stats.DistinctCount(),
				MinValue:      stats.Min(),
				MaxValue:      stats.Max(),
			},
		},
	}

	m.pageDocs = 0

	buf, err := m.ts.Write(context.TODO(), ph)
	if err != nil {
		return err
	}
	if err := m.updateRowGroup(pth, dataLen, compressedLen, len(buf), count, comp); err != nil {
		return err
	}

	_, err = w.Write(buf)
	return err
}

func (m *Metadata) updateRowGroup(pth []string, dataLen, compressedLen, headerLen, count int, comp sch.CompressionCodec) error {
	i := len(m.rowGroups)
	if i == 0 {
		return fmt.Errorf("no row groups, you must call StartRowGroup at least once")
	}

	rg := m.rowGroups[i-1]

	rg.rowGroup.NumRows = m.rowGroupDocs
	err := rg.updateColumnChunk(pth, dataLen+headerLen, compressedLen+headerLen, count, m.schema, comp)
	m.rowGroups[i-1] = rg
	return err
}

func columnType(col string, fields schema) (sch.Type, error) {
	f, ok := fields.lookup[col]
	if !ok {
		return 0, fmt.Errorf("could not find type for column %s", col)
	}
	return *f.Type, nil
}

// Rows return the total number of rows that are being written
// in to a parquet file.
func (m *Metadata) Rows() int64 {
	return m.metadata.NumRows
}

// Footer writes the FileMetaData at the end of the file.
func (m *Metadata) Footer(w io.Writer) error {
	_, s := m.schema.schema()
	fmd := &sch.FileMetaData{
		Version:   1,
		Schema:    s,
		NumRows:   m.docs,
		RowGroups: make([]*sch.RowGroup, 0, len(m.rowGroups)),
	}

	pos := int64(4)
	for _, mrg := range m.rowGroups {
		rg := mrg.rowGroup
		if rg.NumRows == 0 {
			continue
		}

		for _, col := range mrg.fields.fields {
			ch, ok := mrg.columns[strings.Join(col.Path, ".")]
			if !ok {
				continue
			}

			ch.FileOffset = pos
			ch.MetaData.DataPageOffset = pos
			rg.TotalByteSize += ch.MetaData.TotalCompressedSize
			rg.Columns = append(rg.Columns, &ch)
			pos += ch.MetaData.TotalCompressedSize
		}

		fmd.RowGroups = append(fmd.RowGroups, &rg)
	}

	for i, f :=  range fmd.Schema {
		fmt.Printf("[%]: converted:%v, logical: %v\n", i, f.ConvertedType, f.LogicalType)
	}
	buf, err := m.ts.Write(context.TODO(), fmd)
	if err != nil {
		return err
	}

	n, err := w.Write(buf)
	if err != nil {
		return err
	}

	return binary.Write(w, binary.LittleEndian, uint32(n))
}

// RowGroup wraps schema.RowGroup and adds accounting functions
// that are used to keep track of number of rows written, byte size,
// etc.
type RowGroup struct {
	fields   schema
	rowGroup sch.RowGroup
	columns  map[string]sch.ColumnChunk
	child    *RowGroup

	Rows int64
}

// Columns returns the Columns of the row group.
func (r *RowGroup) Columns() []*sch.ColumnChunk {
	return r.rowGroup.Columns
}

func (r *RowGroup) updateColumnChunk(pth []string, dataLen, compressedLen, count int, fields schema, comp sch.CompressionCodec) error {
	col := strings.Join(pth, ".")

	ch, ok := r.columns[col]
	if !ok {
		t, err := columnType(col, fields)
		if err != nil {
			return err
		}

		ch = sch.ColumnChunk{
			MetaData: &sch.ColumnMetaData{
				Type:         t,
				Encodings:    []sch.Encoding{sch.Encoding_PLAIN},
				PathInSchema: pth,
				Codec:        comp,
			},
		}
	}

	ch.MetaData.NumValues += int64(count)
	ch.MetaData.TotalUncompressedSize += int64(dataLen)
	ch.MetaData.TotalCompressedSize += int64(compressedLen)
	r.columns[col] = ch
	return nil
}

func schemaElements(fields []Field) schema {
	m := make(map[string]sch.SchemaElement)
	for _, f := range fields {
		var z int32
		se := sch.SchemaElement{
			Name:          f.Name,
			TypeLength:    &z,
			Scale:         &z,
			Precision:     &z,
			FieldID:       &z,
			LogicalType:   f.LogicalType,
			ConvertedType: f.ConvertedType,
		}
		f.Type(&se)
		f.RepetitionType(&se)
		m[strings.Join(f.Path, ".")] = se
	}

	return schema{lookup: m, fields: fields}
}

// Pages maps each column name to its Pages
func (m *Metadata) Pages() (map[string][]Page, error) {
	if len(m.metadata.RowGroups) == 0 {
		return nil, nil
	}
	out := map[string][]Page{}
	for _, rg := range m.metadata.RowGroups {
		for _, ch := range rg.Columns {
			pth := ch.MetaData.PathInSchema
			_, ok := m.schema.lookup[strings.Join(pth, ".")]
			if !ok {
				return nil, fmt.Errorf("could not find schema for %v", pth)
			}

			pg := Page{
				N:      int(ch.MetaData.NumValues),
				Offset: ch.FileOffset,
				Size:   int(ch.MetaData.TotalCompressedSize),
				Codec:  ch.MetaData.Codec,
			}
			k := strings.Join(pth, ".")
			out[k] = append(out[k], pg)
		}
	}
	return out, nil
}

// ReadMetaData reads the FileMetaData from the end of a parquet file
func ReadMetaData(r io.ReadSeeker) (*sch.FileMetaData, error) {
	p := thrift.NewTCompactProtocol(&thrift.StreamTransport{Reader: r})
	size, err := getMetaDataSize(r)
	if err != nil {
		return nil, err
	}

	_, err = r.Seek(-int64(size+8), io.SeekEnd)
	if err != nil {
		return nil, err
	}

	m := sch.NewFileMetaData()
	return m, m.Read(p)
}

// ReadFooter reads the parquet metadata
func (m *Metadata) ReadFooter(r io.ReadSeeker) error {
	meta, err := ReadMetaData(r)
	m.metadata = meta
	return err
}

// PageHeader reads the page header from a column page
func PageHeader(r io.Reader) (*sch.PageHeader, error) {
	p := thrift.NewTCompactProtocol(&thrift.StreamTransport{Reader: r})
	pg := &sch.PageHeader{}
	err := pg.Read(p)
	return pg, err
}

// PageHeaders reads all the page headers without reading the actual
// data.  It is used by parquetgen to print the page headers.
func PageHeaders(footer *sch.FileMetaData, r io.ReadSeeker) ([]sch.PageHeader, error) {
	var pageHeaders []sch.PageHeader
	for _, rg := range footer.RowGroups {
		for _, col := range rg.Columns {
			h, err := PageHeadersAtOffset(r, col.MetaData.DataPageOffset, col.MetaData.NumValues)
			if err != nil {
				return nil, err
			}
			pageHeaders = append(pageHeaders, h...)
		}
	}
	return pageHeaders, nil
}

// PageHeadersAtOffset seeks to the given offset, then reads the PageHeader
// without reading the data.
func PageHeadersAtOffset(r io.ReadSeeker, o, n int64) ([]sch.PageHeader, error) {
	var out []sch.PageHeader
	var nRead int64
	_, err := r.Seek(o, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("unable to seek to offset %d, err: %s", o, err)
	}

	var readOne bool
	if n > 0 {
		readOne = true
	}

	for !readOne || nRead < n {
		if n == 0 {
			readOne = true
		}
		rc := &readCounter{r: r}
		ph, err := PageHeader(rc)
		if err != nil {
			return nil, fmt.Errorf("unable to read page header: %s", err)
		}
		out = append(out, *ph)
		_, err = r.Seek(int64(ph.CompressedPageSize), io.SeekCurrent)
		if err != nil {
			return nil, fmt.Errorf("unable to seek to next page: %s", err)
		}

		nRead += int64(ph.DataPageHeader.NumValues)
	}
	return out, nil
}

// FieldFunc is used to set some of the metadata for each column
type FieldFunc func(*sch.SchemaElement)

// RepetitionRequired sets the repetition type to required
func RepetitionRequired(se *sch.SchemaElement) {
	t := sch.FieldRepetitionType_REQUIRED
	se.RepetitionType = &t
}

// RepetitionOptional sets the repetition type to optional
func RepetitionOptional(se *sch.SchemaElement) {
	t := sch.FieldRepetitionType_OPTIONAL
	se.RepetitionType = &t
}

// RepetitionRepeated sets the repetition type to repeated
func RepetitionRepeated(se *sch.SchemaElement) {
	t := sch.FieldRepetitionType_REPEATED
	se.RepetitionType = &t
}

var fieldFuncs = []FieldFunc{RepetitionRequired, RepetitionOptional, RepetitionRepeated}

// GetBools reads a byte array and turns each bit into a bool
func GetBools(r io.Reader, n int, pageSizes []int) ([]bool, error) {
	var vals [8]bool
	data, _ := ioutil.ReadAll(r)
	out := make([]bool, 0, n)
	for _, nVals := range pageSizes {

		if nVals == 0 {
			continue
		}

		l := (nVals / 8)
		if nVals%8 > 0 {
			l++
		}

		var i int
		chunk := data[:l]
		data = data[l:]
		for _, b := range chunk {
			vals = unpackBools(b)
			m := min(nVals, 8)
			for j := 0; j < m; j++ {
				out = append(out, vals[j])
			}
			i += m
			nVals -= m
		}
	}
	return out, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func unpackBools(data byte) [8]bool {
	x := uint8(data)
	return [8]bool{
		(x>>0)&1 == 1,
		(x>>1)&1 == 1,
		(x>>2)&1 == 1,
		(x>>3)&1 == 1,
		(x>>4)&1 == 1,
		(x>>5)&1 == 1,
		(x>>6)&1 == 1,
		(x>>7)&1 == 1,
	}
}

func getMetaDataSize(r io.ReadSeeker) (int, error) {
	_, err := r.Seek(-8, io.SeekEnd)
	if err != nil {
		return 0, err
	}

	var size uint32
	return int(size), binary.Read(r, binary.LittleEndian, &size)
}
