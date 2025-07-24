package main

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"
)

// PauseController 用于控制暂停/继续
type PauseController struct {
	paused int32 // 使用 atomic 操作
	mu     sync.Mutex
	cond   *sync.Cond
}

func NewPauseController() *PauseController {
	pc := &PauseController{}
	pc.cond = sync.NewCond(&pc.mu)
	return pc
}

func (pc *PauseController) Toggle() {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if atomic.LoadInt32(&pc.paused) == 0 {
		atomic.StoreInt32(&pc.paused, 1)
	} else {
		atomic.StoreInt32(&pc.paused, 0)
		pc.cond.Broadcast()
	}
}

func (pc *PauseController) WaitIfPaused() {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	for atomic.LoadInt32(&pc.paused) == 1 {
		pc.cond.Wait()
	}
}

func (pc *PauseController) IsPaused() bool {
	return atomic.LoadInt32(&pc.paused) == 1
}

// SpeedTracker 用于跟踪传输速度
type SpeedTracker struct {
	mu           sync.Mutex
	totalBytes   int64
	lastBytes    int64
	lastTime     time.Time
	currentSpeed float64 // bytes per second
}

func NewSpeedTracker() *SpeedTracker {
	return &SpeedTracker{
		lastTime: time.Now(),
	}
}

func (st *SpeedTracker) Update(bytes int64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.totalBytes += bytes
	now := time.Now()

	// 每500ms更新一次速度计算
	if now.Sub(st.lastTime) >= 500*time.Millisecond {
		elapsed := now.Sub(st.lastTime).Seconds()
		if elapsed > 0 {
			st.currentSpeed = float64(st.totalBytes-st.lastBytes) / elapsed
		}
		st.lastBytes = st.totalBytes
		st.lastTime = now
	}
}

func (st *SpeedTracker) GetSpeed() float64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.currentSpeed
}

func (st *SpeedTracker) GetSpeedString() string {
	speed := st.GetSpeed()
	if speed < 1024 {
		return fmt.Sprintf("%.0f B/s", speed)
	} else if speed < 1024*1024 {
		return fmt.Sprintf("%.1f KB/s", speed/1024)
	} else if speed < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB/s", speed/1024/1024)
	} else {
		return fmt.Sprintf("%.1f GB/s", speed/1024/1024/1024)
	}
}

// BufferedWriter 提供带缓冲区的写入器
type BufferedWriter struct {
	writer io.Writer
	buffer []byte
	offset int
}

func NewBufferedWriter(writer io.Writer, bufSize int) *BufferedWriter {
	return &BufferedWriter{
		writer: writer,
		buffer: make([]byte, bufSize),
		offset: 0,
	}
}

func (bw *BufferedWriter) Write(p []byte) (n int, err error) {
	n = len(p)
	remaining := len(p)
	srcOffset := 0

	for remaining > 0 {
		available := len(bw.buffer) - bw.offset
		if available == 0 {
			// 缓冲区已满，刷新
			if err = bw.Flush(); err != nil {
				return n - remaining, err
			}
			available = len(bw.buffer)
		}

		copySize := remaining
		if copySize > available {
			copySize = available
		}

		copy(bw.buffer[bw.offset:], p[srcOffset:srcOffset+copySize])
		bw.offset += copySize
		srcOffset += copySize
		remaining -= copySize
	}

	return n, nil
}

func (bw *BufferedWriter) Flush() error {
	if bw.offset == 0 {
		return nil
	}

	_, err := bw.writer.Write(bw.buffer[:bw.offset])
	bw.offset = 0
	return err
}

// AsyncSHA256Writer 在单独的goroutine中异步计算SHA256哈希值
type AsyncSHA256Writer struct {
	writer   io.Writer
	hasher   hash.Hash
	dataCh   chan []byte
	doneCh   chan struct{}
	resultCh chan string
	wg       sync.WaitGroup
	mu       sync.Mutex
	closed   bool
}

func NewAsyncSHA256Writer(writer io.Writer) *AsyncSHA256Writer {
	asw := &AsyncSHA256Writer{
		writer:   writer,
		hasher:   sha256.New(),
		dataCh:   make(chan []byte, 10), // 缓冲通道提高性能
		doneCh:   make(chan struct{}),
		resultCh: make(chan string, 1),
	}

	// 启动哈希计算goroutine
	asw.wg.Add(1)
	go asw.hashWorker()

	return asw
}

func (asw *AsyncSHA256Writer) hashWorker() {
	defer asw.wg.Done()

	for {
		select {
		case data := <-asw.dataCh:
			if data == nil {
				// 收到结束信号
				asw.resultCh <- hex.EncodeToString(asw.hasher.Sum(nil))
				return
			}
			asw.hasher.Write(data)
		case <-asw.doneCh:
			// 强制结束
			return
		}
	}
}

func (asw *AsyncSHA256Writer) Write(p []byte) (n int, err error) {
	// 先写入到目标writer
	n, err = asw.writer.Write(p)
	if err != nil {
		return n, err
	}

	// 异步发送数据给哈希计算器
	asw.mu.Lock()
	if !asw.closed && n > 0 {
		// 创建数据副本以避免竞态条件
		dataCopy := make([]byte, n)
		copy(dataCopy, p[:n])

		// 阻塞等待直到数据被发送到通道
		asw.dataCh <- dataCopy
	}
	asw.mu.Unlock()

	return n, nil
}

func (asw *AsyncSHA256Writer) Close() {
	asw.mu.Lock()
	if !asw.closed {
		asw.closed = true
		// 发送结束信号
		asw.dataCh <- nil
	}
	asw.mu.Unlock()
}

func (asw *AsyncSHA256Writer) Sum() string {
	asw.Close()

	// 等待哈希计算完成
	select {
	case result := <-asw.resultCh:
		return result
	case <-time.After(30 * time.Second): // 超时保护
		close(asw.doneCh)
		asw.wg.Wait()
		return ""
	}
}

// readLines 从指定文件中读取所有行，并去除每行首尾的引号和空白
func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.Trim(line, "\"") // 去除可能存在的引号
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

func main() {
	var noSha256 bool
	flag.BoolVar(&noSha256, "n", false, "跳过SHA256计算")
	flag.Parse()

	// 记录开始时间
	startTime := time.Now()

	// 配置日志记录器
	log.SetFlags(log.LstdFlags) // 设置日志格式为 YYYY/MM/DD HH:MM:SS

	// 1. 从 src.txt 和 dst.txt 读取配置
	sources, err := readLines("src.txt")
	if err != nil {
		log.Fatalf("错误: 无法读取源文件列表 src.txt: %v", err)
	}
	if len(sources) == 0 {
		log.Fatalln("错误: src.txt 为空或不存在。")
	}

	destLines, err := readLines("dst.txt")
	if err != nil {
		log.Fatalf("错误: 无法读取目标文件配置 dst.txt: %v", err)
	}
	if len(destLines) == 0 {
		log.Fatalln("错误: dst.txt 为空或不存在。")
	}
	destFile := destLines[0]

	// 确保不会将输出文件打包到自身
	absDest, err := filepath.Abs(destFile)
	if err != nil {
		log.Fatalf("错误: 无法获取目标绝对路径: %v", err)
	}
	for _, source := range sources {
		absSource, err := filepath.Abs(source)
		if err != nil {
			log.Fatalf("错误: 无法获取源 '%s' 的绝对路径: %v", source, err)
		}

		// Check if source is a directory and add a separator
		info, err := os.Stat(absSource)
		if err == nil && info.IsDir() {
			if !strings.HasSuffix(absSource, string(os.PathSeparator)) {
				absSource += string(os.PathSeparator)
			}
		}

		if strings.HasPrefix(absDest, absSource) {
			log.Fatalf("错误: 目标zip文件 '%s' 不能位于源目录 '%s' 中。", destFile, source)
		}
	}

	// --- 阶段 1: 扫描文件以统计总数和大小 ---
	log.Println("阶段 1/2: 正在扫描文件...")
	var totalFiles int64
	var totalSize int64
	for _, source := range sources {
		err := filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				log.Printf("访问 %s 时发生错误: %v", path, err)
				return nil // 继续处理其他文件
			}
			if !info.IsDir() {
				totalFiles++
				totalSize += info.Size()
			}
			return nil
		})
		if err != nil {
			log.Printf("错误: 扫描文件 '%s' 时出错: %v", source, err)
		}
	}
	log.Printf("扫描完成。共找到 %d 个文件, 总大小 %.2f MB\n", totalFiles, float64(totalSize)/1024/1024)

	// --- 阶段 2: 执行压缩并显示进度条 ---
	log.Println("阶段 2/2: 开始压缩文件...")
	log.Println("提示: 按回车键可以暂停/继续压缩过程")

	file, err := os.Create(destFile)
	if err != nil {
		log.Fatalf("错误: 无法创建目标文件 %s: %v", destFile, err)
	}
	defer file.Close()

	// 初始化暂停控制器
	pauseController := NewPauseController()

	// 初始化速度跟踪器
	speedTracker := NewSpeedTracker()

	// 用于在 goroutine 之间共享当前处理的文件名
	var currentFile atomic.Value
	currentFile.Store("") // 初始化为空字符串

	// 初始化进度条
	bar := progressbar.NewOptions64(
		totalSize,
		progressbar.OptionSetWriter(os.Stderr), // 明确指定输出到 stderr
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(15),
		progressbar.OptionThrottle(200*time.Millisecond), // 稍微降低更新频率
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprint(os.Stderr, "\n")
		}),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionClearOnFinish(), // 完成后清除进度条
	)

	// 启动协程监听键盘输入
	go func() {
		// 确保在程序退出时能关闭标准输入，让 goroutine 结束
		defer func() {
			_ = os.Stdin.Close()
		}()
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			pauseController.Toggle()
		}
	}()

	// 启动一个协程定期更新进度条描述以显示速度、暂停状态和当前文件
	done := make(chan bool)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond) // 更新频率设为 200ms
		defer ticker.Stop()

		// 获取当前文件名并缩短
		baseName := filepath.Base(destFile)
		maxBaseNameLen := 16 // 路径最大显示长度
		if len(baseName) > maxBaseNameLen {
			baseName = "..." + baseName[len(baseName)-maxBaseNameLen+3:]
		}
		for {
			select {
			case <-ticker.C:
				speedStr := speedTracker.GetSpeedString()
				var statusStr string
				if pauseController.IsPaused() {
					statusStr = "[已暂停]"
				} else {
					statusStr = fmt.Sprintf("[%s]", speedStr)
				}

				// 获取当前文件名并缩短
				filePath := ""
				if cf := currentFile.Load(); cf != nil {
					filePath = cf.(string)
				}
				maxPathLen := 16 // 路径最大显示长度
				if len(filePath) > maxPathLen {
					filePath = "..." + filePath[len(filePath)-maxPathLen+3:]
				}

				newDesc := fmt.Sprintf("压缩中 %s %s %s", statusStr, baseName, filePath)
				bar.Describe(newDesc)
			case <-done:
				bar.Describe(fmt.Sprintf("压缩完成: %s", baseName))
				return
			}
		}
	}()

	// 根据是否需要计算SHA256选择写入器
	var finalWriter io.Writer
	var sha256Writer *AsyncSHA256Writer

	if noSha256 {
		finalWriter = file
	} else {
		sha256Writer = NewAsyncSHA256Writer(file)
		finalWriter = sha256Writer
	}

	// 创建带缓冲的文件写入器
	bufferedFile := NewBufferedWriter(finalWriter, 10*1024*1024) // 10MB buffer

	// 创建 Zip Writer
	zipWriter := zip.NewWriter(bufferedFile)
	defer func() {
		zipWriter.Close()
		bufferedFile.Flush()
	}()

	// 用于跟踪已经创建的卷根目录
	createdVolumes := make(map[string]bool)

	// 遍历所有源，将它们添加到zip中
	for _, source := range sources {
		if err := addFiles(zipWriter, source, bar, speedTracker, pauseController, &currentFile, createdVolumes); err != nil {
			done <- true // 发生错误，通知更新 goroutine 停止
			// 在新行打印错误，避免与进度条混淆
			fmt.Fprintf(os.Stderr, "\n")
			log.Fatalf("错误: 压缩 '%s' 过程中发生错误: %v", source, err)
		}
	}

	// 确保所有数据都被刷新到文件
	zipWriter.Close()
	bufferedFile.Flush()

	// 获取计算出的SHA256值
	var sha256Sum string
	if !noSha256 && sha256Writer != nil {
		sha256Sum = sha256Writer.Sum()
	}

	done <- true // 通知进度条更新 goroutine 退出
	bar.Finish() // 确保进度条达到100%

	// 计算并打印总耗时
	duration := time.Since(startTime)
	log.Printf("压缩完成。总共用时: %.2f 秒", duration.Seconds())

	if !noSha256 {
		if sha256Sum != "" {
			log.Printf("SHA256 校验和: %s", sha256Sum)

			// 将SHA256写入到同名的.sha256文件中
			sha256File := destFile + ".sha256"
			if err := os.WriteFile(sha256File, []byte(sha256Sum+"  "+filepath.Base(destFile)+"\n"), 0644); err != nil {
				log.Printf("警告: 无法写入SHA256文件 %s: %v", sha256File, err)
			} else {
				log.Printf("SHA256校验和已保存到: %s", sha256File)
			}
		} else {
			log.Printf("警告: SHA256计算超时或失败")
		}
	}
}

// addFiles 遍历路径并将其中的文件和目录添加到zip.Writer中
func addFiles(w *zip.Writer, basePath string, bar *progressbar.ProgressBar,
	speedTracker *SpeedTracker, pauseController *PauseController, currentFile *atomic.Value,
	createdVolumes map[string]bool) error {

	copyBuffer := make([]byte, 5*1024*1024) // 5MB缓冲区

	return filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("访问 %s 时发生错误: %v", path, err)
			return nil // 继续处理其他文件
		}

		pauseController.WaitIfPaused()

		// 更新当前正在处理的文件名，供进度条显示
		currentFile.Store(filepath.Base(path))

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			log.Printf("无法获取文件头信息 %s: %v", path, err)
			return nil // 继续处理其他文件
		}

		absPath, err := filepath.Abs(path)
		if err != nil {
			log.Printf("无法获取绝对路径 %s: %v", path, err)
			return nil
		}
		volumeName := filepath.VolumeName(absPath)         // e.g., "C:"
		driveLetter := strings.TrimSuffix(volumeName, ":") // e.g., "C"

		// 创建卷的根目录 (e.g., "C/")，仅创建一次
		if driveLetter != "" && !createdVolumes[driveLetter] {
			// 创建一个虚拟的 FileInfo 用于 FileInfoHeader
			volHeader, _ := zip.FileInfoHeader(info)
			volHeader.Name = driveLetter + "/"
			volHeader.Method = zip.Store
			if _, err := w.CreateHeader(volHeader); err != nil {
				log.Printf("无法为卷创建目录 %s: %v", volHeader.Name, err)
			}
			createdVolumes[driveLetter] = true
		}

		// 从绝对路径中移除卷名和前导分隔符
		pathWithoutVolume := strings.TrimPrefix(absPath, volumeName)
		pathWithoutVolume = strings.TrimPrefix(pathWithoutVolume, string(os.PathSeparator))

		// 将盘符和剩余路径结合起来
		zipPath := filepath.Join(driveLetter, pathWithoutVolume)

		// 如果zipPath为空（例如，当path和baseDir相同时），则跳过
		if zipPath == "" || zipPath == "." {
			return nil
		}

		header.Name = filepath.ToSlash(zipPath)
		header.Method = zip.Store // 不压缩

		if info.IsDir() {
			header.Name += "/"
		}

		writer, err := w.CreateHeader(header)
		if err != nil {
			log.Printf("无法在zip中创建文件头 %s: %v", header.Name, err)
			return nil
		}

		if !info.IsDir() {
			file, err := os.Open(path)
			if err != nil {
				log.Printf("无法打开文件 %s: %v", path, err)
				return nil
			}
			defer file.Close()

			for {
				pauseController.WaitIfPaused()

				n, readErr := file.Read(copyBuffer)
				if n > 0 {
					if _, writeErr := writer.Write(copyBuffer[:n]); writeErr != nil {
						log.Printf("写入zip文件时出错 %s: %v", path, writeErr)
						return nil
					}
					bar.Add(n)
					speedTracker.Update(int64(n))
				}
				if readErr != nil {
					if readErr == io.EOF {
						break
					}
					log.Printf("读取文件时出错 %s: %v", path, readErr)
					return nil
				}
			}
		}
		return nil
	})
}
