package virtual

import (
	"bytes"
	"context"
	"errors"
	"io"
	"unicode/utf8"
)

const scanBufferBytes = 64 << 10

func buildLogicalPages(ctx context.Context, source Source, length int64, options resolvedOptions) ([]pageMeta, error) {
	buffer := make([]byte, scanBufferBytes)
	pending := make([]byte, 0, utf8.UTFMax-1)
	pages := make([]pageMeta, 0)
	var readOffset, pageStart, pageStartLine, line int64
	previousRuneLF := false

	appendPage := func(end, endLine int64, endsWithLF bool) {
		continuesFrom := pageStart > 0 && len(pages) > 0 && !pages[len(pages)-1].endsWithLF
		pages = append(pages, pageMeta{
			start: pageStart, end: end, startLine: pageStartLine, endLine: endLine,
			continuesFrom: continuesFrom, continuesTo: end < length && !endsWithLF,
			endsWithLF: endsWithLF, fragment: -1,
		})
		pageStart, pageStartLine = end, endLine
	}

	for readOffset < length {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		want := min(int64(len(buffer)), length-readOffset)
		n, readErr := source.ReadAt(buffer[:int(want)], readOffset)
		if n < 0 || int64(n) > want {
			return nil, ErrSourceInconsistent
		}
		data := make([]byte, 0, len(pending)+n)
		data = append(data, pending...)
		data = append(data, buffer[:n]...)
		base := readOffset - int64(len(pending))
		pending = pending[:0]
		for cursor := 0; cursor < len(data); {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !utf8.FullRune(data[cursor:]) {
				pending = append(pending, data[cursor:]...)
				break
			}
			r, size := utf8.DecodeRune(data[cursor:])
			if r == utf8.RuneError && size == 1 {
				return nil, ErrInvalidUTF8
			}
			runeStart := base + int64(cursor)
			runeEnd := runeStart + int64(size)
			if runeEnd-pageStart > options.maximumPageBytes {
				appendPage(runeStart, line, previousRuneLF)
			}
			if r == '\n' {
				line++
			}
			previousRuneLF = r == '\n'
			if runeEnd-pageStart >= options.targetPageBytes && r == '\n' {
				appendPage(runeEnd, line, true)
			}
			cursor += size
		}
		readOffset += int64(n)
		if readErr != nil && !(errors.Is(readErr, io.EOF) && readOffset == length) {
			return nil, readErr
		}
		if n == 0 && readOffset < length {
			return nil, io.ErrUnexpectedEOF
		}
	}
	if len(pending) != 0 {
		return nil, ErrInvalidUTF8
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pageStart < length {
		appendPage(length, line, previousRuneLF)
	}
	if len(pages) == 0 {
		pages = append(pages, pageMeta{fragment: -1})
	}
	return pages, nil
}

func readExactAt(ctx context.Context, source Source, data []byte, offset int64) error {
	for read := 0; read < len(data); {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := source.ReadAt(data[read:], offset+int64(read))
		if n < 0 || n > len(data)-read {
			return ErrSourceInconsistent
		}
		read += n
		if err != nil && !(errors.Is(err, io.EOF) && read == len(data)) {
			return err
		}
		if n == 0 && read < len(data) {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

func readPageBytes(ctx context.Context, source Source, page pageMeta) ([]byte, error) {
	length := page.end - page.start
	if length < 0 || length > int64(int(^uint(0)>>1)) {
		return nil, ErrSourceInconsistent
	}
	data := make([]byte, int(length))
	if err := readExactAt(ctx, source, data, page.start); err != nil {
		return nil, err
	}
	return data, nil
}

func lineAndBoundaryMap(ctx context.Context, source Source, logical []pageMeta, extra []int64) (map[int64]int64, map[int64]bool, error) {
	lines := make(map[int64]int64, len(logical)+len(extra)+1)
	afterLF := make(map[int64]bool, len(logical)+len(extra)+1)
	lines[0], afterLF[0] = 0, true
	extraIndex := 0
	for _, page := range logical {
		lines[page.start] = page.startLine
		if page.start == 0 {
			afterLF[page.start] = true
		} else {
			afterLF[page.start] = !page.continuesFrom
		}
		internalStart := extraIndex
		for extraIndex < len(extra) && extra[extraIndex] <= page.start {
			extraIndex++
		}
		internalStart = extraIndex
		for extraIndex < len(extra) && extra[extraIndex] < page.end {
			extraIndex++
		}
		if internalStart < extraIndex {
			data, err := readPageBytes(ctx, source, page)
			if err != nil {
				return nil, nil, err
			}
			cursor := 0
			currentLine := page.startLine
			for _, offset := range extra[internalStart:extraIndex] {
				position := int(offset - page.start)
				if position < len(data) && data[position]&0xc0 == 0x80 {
					return nil, nil, ErrInvalidFragment
				}
				currentLine += int64(bytes.Count(data[cursor:position], []byte{'\n'}))
				lines[offset] = currentLine
				afterLF[offset] = position == 0 || data[position-1] == '\n'
				cursor = position
			}
		}
		lines[page.end] = page.endLine
		afterLF[page.end] = page.endsWithLF
	}
	return lines, afterLF, nil
}
