package zipconduit

import (
	"archive/zip"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/danielpaulus/go-ios/ios"
	log "github.com/sirupsen/logrus"
)

/*
*
Typical weird iOS service :-D
It is a kind of special "zip" format that XCode uses to send files&folder to devices.
Sadly it is not compliant with all standard zip libraries, in particular it does not work
with the golang zipWriter implementation... OF COURSE ;-)
This is why I had to hack my own "zip" encoding together. Here is how zip_conduit works:

 1. Send PLIST "InitTransfer" in standard 4byte length + Plist format
 2. Start sending binary zip stream next
 3. Since zip does not support streaming,
    we first generate a metainf file inside a metainf directory. It contains number of files and
    total byte sizes among other things (check the struct). Probably to make streaming work we also send
    it as the first file
 4. Starting with metainf for each file:
    send a ZipFileHeader with compression set to STORE (so no compression at all)
    this also means uncompressedSize==compressedSize btw.
    be sure not to use DataDescriptors (https://en.wikipedia.org/wiki/ZIP_(file_format)#Local_file_header)
    I guess they have disabled them as it would make streaming harder. This is why golang's zip implementation
    does not work.
 5. Send the standard central directory header but not a central directory (obviously)
 6. wait for a bunch of PLISTs to be received that indicate progress and completion of installation
*/
const (
	usbmuxdServiceName string = "com.apple.streaming_zip_conduit"
	shimServiceName    string = "com.apple.streaming_zip_conduit.shim.remote"
)

// Connection exposes functions to interoperate with zipconduit
type Connection struct {
	deviceConn io.ReadWriteCloser
	plistCodec ios.PlistCodec
}

// New returns a new ZipConduit Connection for the given DeviceID and Udid
func New(device ios.DeviceEntry) (*Connection, error) {
	if !device.SupportsRsd() {
		return NewWithUsbmuxdConnection(device)
	}
	return NewWithShimConnection(device)
}

// NewWithUsbmuxdConnection connects to the streaming_zip_conduit service on the device over the usbmuxd socket
func NewWithUsbmuxdConnection(device ios.DeviceEntry) (*Connection, error) {
	deviceConn, err := ios.ConnectToService(device, usbmuxdServiceName)
	if err != nil {
		return &Connection{}, err
	}

	return &Connection{
		deviceConn: deviceConn,
		plistCodec: ios.NewPlistCodec(),
	}, nil
}

// NewWithShimConnection connects to the streaming_zip_conduit service over a tunnel interface and the service port
// is obtained from remote service discovery
func NewWithShimConnection(device ios.DeviceEntry) (*Connection, error) {
	deviceConn, err := ios.ConnectToShimService(device, shimServiceName)
	if err != nil {
		return &Connection{}, err
	}

	return &Connection{
		deviceConn: deviceConn,
		plistCodec: ios.NewPlistCodec(),
	}, nil
}

// SendFile will send either a zipFile or an unzipped directory to the device.
// If you specify appFilePath to a file, it will try to Unzip it to a temp dir first and then send.
// If appFilePath points to a directory, it will try to install the dir contents as an app.
func (conn Connection) SendFile(appFilePath string) error {
	openedFile, err := os.Open(appFilePath)
	if err != nil {
		return err
	}

	// Get the file information
	info, err := openedFile.Stat()
	openedFile.Close()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return conn.sendDirectory(appFilePath)
	}
	return conn.sendIpaFile(appFilePath)
}

func (conn Connection) Close() error {
	return conn.deviceConn.Close()
}

func (conn Connection) sendDirectory(dir string) error {
	tmpDir, err := os.MkdirTemp("", "prefix")
	if err != nil {
		return err
	}
	log.Debugf("created tempdir: %s", tmpDir)
	defer func() {
		err := os.RemoveAll(tmpDir)
		if err != nil {
			log.WithFields(log.Fields{"dir": tmpDir}).Warn("failed removing tempdir")
		}
	}()
	var totalBytes int64
	var unzippedFiles []string
	err = filepath.Walk(dir,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			totalBytes += info.Size()
			unzippedFiles = append(unzippedFiles, path)
			return nil
		})
	if err != nil {
		return err
	}

	metainfFolder, metainfFile, err := addMetaInf(tmpDir, unzippedFiles, uint64(totalBytes))
	if err != nil {
		return err
	}

	init := newInitTransfer(dir + ".ipa")
	log.Debugf("sending inittransfer %+v", init)
	bytes, err := conn.plistCodec.Encode(init)
	if err != nil {
		return err
	}

	_, err = conn.deviceConn.Write(bytes)
	if err != nil {
		return err
	}

	log.Debug("writing meta inf")
	err = AddFileToZip(conn.deviceConn, metainfFolder, tmpDir)
	if err != nil {
		return err
	}
	err = AddFileToZip(conn.deviceConn, metainfFile, tmpDir)
	if err != nil {
		return err
	}
	log.Debug("meta inf send successfully")

	log.Debug("sending files....")

	for _, file := range unzippedFiles {
		err := AddFileToZip(conn.deviceConn, file, dir)
		if err != nil {
			return err
		}
	}
	log.Debug("files sent, sending central header....")
	_, err = conn.deviceConn.Write(centralDirectoryHeader)
	if err != nil {
		return err
	}

	return conn.waitForInstallation()
}

func (conn Connection) sendIpaFile(ipaFile string) error {
	// Open the IPA (ZIP) file for reading
	zipReader, err := zip.OpenReader(ipaFile)
	if err != nil {
		return err
	}
	defer zipReader.Close()

	// Calculate total uncompressed size and count files
	var totalBytes uint64
	fileCount := 0
	for _, f := range zipReader.File {
		totalBytes += f.UncompressedSize64
		fileCount++
	}

	// Create temporary directory only for metainf
	tmpDir, err := os.MkdirTemp("", "prefix")
	if err != nil {
		return err
	}
	log.Debugf("created tempdir for metainf: %s", tmpDir)
	defer func() {
		err := os.RemoveAll(tmpDir)
		if err != nil {
			log.WithFields(log.Fields{"dir": tmpDir}).Warn("failed removing tempdir")
		}
	}()

	// Create metainf with file count from ZIP
	metainfFolder, metainfFile, err := addMetaInfForZip(tmpDir, fileCount, totalBytes)
	if err != nil {
		return err
	}

	init := newInitTransfer(ipaFile)
	log.Debugf("sending inittransfer %+v", init)
	bytes, err := conn.plistCodec.Encode(init)
	if err != nil {
		return err
	}

	_, err = conn.deviceConn.Write(bytes)
	if err != nil {
		return err
	}

	log.Debug("writing meta inf")
	err = AddFileToZip(conn.deviceConn, metainfFolder, tmpDir)
	if err != nil {
		return err
	}
	err = AddFileToZip(conn.deviceConn, metainfFile, tmpDir)
	if err != nil {
		return err
	}
	log.Debug("meta inf send successfully")

	log.Debug("sending files from IPA archive....")

	// Stream files directly from ZIP archive
	for _, f := range zipReader.File {
		err := conn.addZipFileToStream(f)
		if err != nil {
			return err
		}
	}
	log.Debug("files sent, sending central header....")
	_, err = conn.deviceConn.Write(centralDirectoryHeader)
	if err != nil {
		return err
	}

	return conn.waitForInstallation()
}

func (conn Connection) waitForInstallation() error {
	for {
		msg, _ := conn.plistCodec.Decode(conn.deviceConn)
		plist, _ := ios.ParsePlist(msg)
		log.Debugf("%+v", plist)
		done, percent, status, err := evaluateProgress(plist)
		if err != nil {
			return err
		}
		if done {
			log.Info("installation successful")
			return nil
		}
		log.WithFields(log.Fields{"status": status, "percentComplete": percent}).Info("installing")
	}
}

const metainfFileName = "com.apple.ZipMetadata.plist"

func addMetaInf(metainfPath string, files []string, totalBytes uint64) (string, string, error) {
	folderPath := path.Join(metainfPath, "META-INF")
	ret, _ := ios.PathExists(folderPath)
	if !ret {
		err := os.Mkdir(folderPath, 0o777)
		if err != nil {
			return "", "", err
		}
	}
	// recordcount == files + meta-inf + metainffile
	meta := metadata{RecordCount: 2 + len(files), StandardDirectoryPerms: 16877, StandardFilePerms: -32348, TotalUncompressedBytes: totalBytes, Version: 2}
	metaBytes := ios.ToPlistBytes(meta)
	filePath := path.Join(metainfPath, "META-INF", metainfFileName)
	err := os.WriteFile(filePath, metaBytes, 0o777)
	if err != nil {
		return "", "", err
	}
	return folderPath, filePath, nil
}

func addMetaInfForZip(metainfPath string, fileCount int, totalBytes uint64) (string, string, error) {
	folderPath := path.Join(metainfPath, "META-INF")
	ret, _ := ios.PathExists(folderPath)
	if !ret {
		err := os.Mkdir(folderPath, 0o777)
		if err != nil {
			return "", "", err
		}
	}
	// recordcount == files + meta-inf + metainffile
	meta := metadata{RecordCount: 2 + fileCount, StandardDirectoryPerms: 16877, StandardFilePerms: -32348, TotalUncompressedBytes: totalBytes, Version: 2}
	metaBytes := ios.ToPlistBytes(meta)
	filePath := path.Join(metainfPath, "META-INF", metainfFileName)
	err := os.WriteFile(filePath, metaBytes, 0o777)
	if err != nil {
		return "", "", err
	}
	return folderPath, filePath, nil
}

// addZipFileToStream streams a file directly from the ZIP archive to the device
// using the pre-computed CRC32 from the ZIP metadata
func (conn Connection) addZipFileToStream(f *zip.File) error {
	// Handle directories
	if f.FileInfo().IsDir() {
		header, name, extra := newZipHeaderDir(f.Name)
		if err := binary.Write(conn.deviceConn, binary.LittleEndian, header); err != nil {
			return err
		}
		if err := binary.Write(conn.deviceConn, binary.BigEndian, name); err != nil {
			return err
		}
		return binary.Write(conn.deviceConn, binary.BigEndian, extra)
	}

	// Open file from ZIP archive
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	// Use pre-computed CRC32 from ZIP metadata
	crc := f.CRC32

	// Write ZIP header with pre-computed CRC
	header, name, extra := newZipHeader(uint32(f.UncompressedSize64), crc, f.Name)
	if err := binary.Write(conn.deviceConn, binary.LittleEndian, header); err != nil {
		return err
	}
	if err := binary.Write(conn.deviceConn, binary.BigEndian, name); err != nil {
		return err
	}
	if err := binary.Write(conn.deviceConn, binary.BigEndian, extra); err != nil {
		return err
	}

	// Stream file content directly to device with a small buffer
	buf := make([]byte, 32*1024) // 32KB buffer
	_, err = io.CopyBuffer(conn.deviceConn, rc, buf)
	return err
}

func AddFileToZip(writer io.Writer, filename string, tmpdir string) error {
	fileToZip, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer fileToZip.Close()

	// Get the file information
	info, err := fileToZip.Stat()
	if err != nil {
		return err
	}

	// Using FileInfoHeader() above only uses the basename of the file. If we want
	// to preserve the folder structure we can overwrite this with the full path.
	var filenameForZip string
	if runtime.GOOS == "windows" {
		filenameForZip = strings.Replace(ios.FixWindowsPaths(filename), ios.FixWindowsPaths(tmpdir)+"/", "", 1)
		if info.IsDir() && !strings.HasSuffix(filenameForZip, "/") {
			filenameForZip += "/"
		}
	} else {
		filenameForZip = strings.Replace(filename, tmpdir+"/", "", 1)
		if info.IsDir() && !strings.HasSuffix(filenameForZip, "/") {
			filenameForZip += "/"
		}
	}

	if info.IsDir() {
		// write our "zip" header for a directory
		header, name, extra := newZipHeaderDir(filenameForZip)
		err := binary.Write(writer, binary.LittleEndian, header)
		if err != nil {
			return err
		}
		err = binary.Write(writer, binary.BigEndian, name)
		if err != nil {
			return err
		}
		err = binary.Write(writer, binary.BigEndian, extra)
		return err
	}

	// Calculate CRC32 for files extracted to temp directory
	crc, err := calculateCrc32ForFile(fileToZip)
	if err != nil {
		return err
	}
	fileToZip.Seek(0, io.SeekStart)
	// write our "zip" file header
	header, name, extra := newZipHeader(uint32(info.Size()), crc, filenameForZip)
	err = binary.Write(writer, binary.LittleEndian, header)
	if err != nil {
		return err
	}
	err = binary.Write(writer, binary.BigEndian, name)
	if err != nil {
		return err
	}
	err = binary.Write(writer, binary.BigEndian, extra)
	if err != nil {
		return err
	}
	_, err = io.Copy(writer, fileToZip)
	return err
}

// calculateCrc32ForFile calculates CRC32 for files that are already extracted to temp directory
// This is only used for sendDirectory, not for IPA files which use pre-computed CRC32 from ZIP metadata
func calculateCrc32ForFile(file *os.File) (uint32, error) {
	hash := crc32.New(crc32.IEEETable)
	// Use io.Copy which handles buffering efficiently
	if _, err := io.Copy(hash, file); err != nil {
		return 0, err
	}
	return hash.Sum32(), nil
}
