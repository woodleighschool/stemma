package msi

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Column type bits recorded by the _Columns table.
const (
	columnValid    = 0x0100
	columnString   = 0x0800
	columnNullable = 0x1000
)

type column struct {
	name  string
	width int
	text  bool
}

// columns lists the columns _Columns declares for table, in column order.
// Tables store their values column by column, each as wide as its type: a
// string reference, a 2- or 4-byte integer, or a 2-byte stream placeholder.
func (db *database) columns(strs *stringTable, table string) ([]column, error) {
	data, ok, err := db.decoded("_Columns")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("MSI has no _Columns table")
	}
	ref := strs.bytesPerRef
	if len(data)%(2*ref+4) != 0 {
		return nil, errors.New("_Columns table has an invalid row size")
	}
	rows := len(data) / (2*ref + 4)
	declared := map[int]column{}
	for row := range rows {
		tableIndex, _ := readRef(data, row*ref, ref)
		if strs.get(tableIndex) != table {
			continue
		}
		// Integer columns store a value with its sign bit flipped.
		number := int(binary.LittleEndian.Uint16(data[rows*ref+row*2:])) - 0x8000
		nameIndex, _ := readRef(data, rows*(ref+2)+row*ref, ref)
		kind := int(binary.LittleEndian.Uint16(data[rows*(2*ref+2)+row*2:])) - 0x8000
		col := column{name: strs.get(nameIndex)}
		if _, exists := declared[number]; exists || number < 1 || kind < 0 || col.name == "" {
			return nil, fmt.Errorf("_Columns declares an invalid %s column", table)
		}
		switch {
		case kind&^columnNullable == columnString|columnValid:
			col.width = 2
		case kind&columnString != 0:
			col.width, col.text = ref, true
		case kind&0xff <= 2:
			col.width = 2
		case kind&0xff == 4:
			col.width = 4
		default:
			return nil, fmt.Errorf("%s column %s has unsupported type %#x", table, col.name, kind)
		}
		declared[number] = col
	}
	if len(declared) == 0 {
		return nil, fmt.Errorf("MSI has no %s table", table)
	}
	result := make([]column, len(declared))
	for number, col := range declared {
		if number > len(declared) {
			return nil, fmt.Errorf("_Columns omits a %s column", table)
		}
		result[number-1] = col
	}
	return result, nil
}

// stringRows returns the named string columns of each row of table.
func (db *database) stringRows(table string, names ...string) ([][]string, error) {
	strs, err := db.loadStrings()
	if err != nil {
		return nil, err
	}
	cols, err := db.columns(strs, table)
	if err != nil {
		return nil, err
	}
	type selected struct{ offset, width int }
	chosen := make([]selected, len(names))
	rowSize := 0
	for _, col := range cols {
		for i, name := range names {
			if col.name == name && col.text {
				chosen[i] = selected{offset: rowSize, width: col.width}
			}
		}
		rowSize += col.width
	}
	for i, name := range names {
		if chosen[i].width == 0 {
			return nil, fmt.Errorf("%s table has no %s string column", table, name)
		}
	}
	data, ok, err := db.decoded(table)
	if err != nil || !ok {
		return nil, err
	}
	if len(data)%rowSize != 0 {
		return nil, fmt.Errorf("%s table has an invalid row size", table)
	}
	rows := len(data) / rowSize
	result := make([][]string, rows)
	for row := range rows {
		values := make([]string, len(names))
		for i, col := range chosen {
			index, _ := readRef(data, col.offset*rows+row*col.width, col.width)
			if index >= len(strs.strings) {
				return nil, errors.New("invalid MSI string reference")
			}
			values[i] = strs.get(index)
		}
		result[row] = values
	}
	return result, nil
}
