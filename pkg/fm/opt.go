package fm

type sortMethod byte

const (
	naturalSort sortMethod = iota
	nameSort
	sizeSort
	timeSort
	atimeSort
	ctimeSort
	extSort
)

type sortOption byte

const (
	dirfirstSort sortOption = 1 << iota
	hiddenSort
	reverseSort
)

type sortType struct {
	method sortMethod
	option sortOption
}
