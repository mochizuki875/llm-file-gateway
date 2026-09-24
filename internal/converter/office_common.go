package converter

// File format signatures used to validate Office documents.
var (
	zipSignature = []byte("PK\x03\x04")                                   // OOXML (docx/pptx/xlsx/xlsm) ZIP container
	oleSignature = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1} // Legacy OLE (doc/ppt/xls)
)
