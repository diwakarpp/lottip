package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strconv"
)

var errInvalidPacketLength = errors.New("Invalid packet length")
var errInvalidPacketType = errors.New("Invalid packet type")
var errFieldTypeNotImplementedYet = errors.New("Required field type not implemented yet")

type HandshakeV10 struct {
	ProtocolVersion    byte
	ServerVersion      string
	ConnectionID       uint32
	ServerCapabilities uint32
	AuthPlugin         string
}

func DecodeHandshakeV10(packet []byte) (*HandshakeV10, error) {
	r := bytes.NewReader(packet)

	// Skip packet header
	if _, err := r.Seek(4, io.SeekStart); err != nil {
		return nil, err
	}

	// Read ProtocolVersion value
	protoVersion, _ := r.ReadByte()

	// Read ServerVersion value
	serverVersion := ReadNullTerminatedString(r)

	// Read ConnectionID value
	connectionIDBuf := make([]byte, 4)
	if _, err := r.Read(connectionIDBuf); err != nil {
		return nil, err
	}
	connectionID := binary.LittleEndian.Uint32(connectionIDBuf)

	// Skip AuthPluginData and filler (always 0x00)
	if _, err := r.Seek(9, io.SeekCurrent); err != nil {
		return nil, err
	}

	// Read ServerCapabilities value
	serverCapabilitiesLowerBuf := make([]byte, 2)
	if _, err := r.Read(serverCapabilitiesLowerBuf); err != nil {
		return nil, err
	}

	// Skip ServerDefaultCollation and StatusFlags
	if _, err := r.Seek(3, io.SeekCurrent); err != nil {
		return nil, err
	}

	// Read ExServerCapabilities value
	serverCapabilitiesHigherBuf := make([]byte, 2)
	if _, err := r.Read(serverCapabilitiesHigherBuf); err != nil {
		return nil, err
	}

	// Compose ServerCapabilities from 2 bufs
	var serverCapabilitiesBuf []byte
	serverCapabilitiesBuf = append(serverCapabilitiesBuf, serverCapabilitiesLowerBuf...)
	serverCapabilitiesBuf = append(serverCapabilitiesBuf, serverCapabilitiesHigherBuf...)
	serverCapabilities := binary.LittleEndian.Uint32(serverCapabilitiesBuf)

	var authPluginDataLength byte
	if serverCapabilities&clientPluginAuth != 0 {
		var err error
		authPluginDataLength, err = r.ReadByte()
		if err != nil {
			return nil, err
		}
	}

	// Skip reserved (all 0x00)
	if _, err := r.Seek(10, io.SeekCurrent); err != nil {
		return nil, err
	}

	if serverCapabilities&clientSecureConnection != 0 {
		skip := int64(math.Max(13, float64(authPluginDataLength)-8))
		// Skip reserved (all 0x00)
		if _, err := r.Seek(skip, io.SeekCurrent); err != nil {
			return nil, err
		}
	}

	var authPlugin string
	if serverCapabilities&clientPluginAuth != 0 {
		authPlugin = ReadNullTerminatedString(r)
	}

	return &HandshakeV10{
		ProtocolVersion:    protoVersion,
		ServerVersion:      serverVersion,
		ConnectionID:       connectionID,
		ServerCapabilities: serverCapabilities,
		AuthPlugin:         authPlugin,
	}, nil
}

// QueryRequest represents COM_QUERY or COM_STMT_PREPARE command sent by client to server.
type QueryRequest struct {
	Query string // SQL query value
}

// DecodeQueryRequest decodes COM_QUERY and COM_STMT_PREPARE requests from client.
// Basic packet structure shown below.
// See https://mariadb.com/kb/en/mariadb/com_query/ and https://mariadb.com/kb/en/mariadb/com_stmt_prepare/
//
// int<3> PacketLength
// int<1> PacketNumber
// int<1> Command COM_QUERY (0x03) or COM_STMT_PREPARE (0x16)
// string<EOF> SQLStatement
func DecodeQueryRequest(packet []byte) (*QueryRequest, error) {

	// Min packet length = header(4 bytes) + command(1 byte) + SQLStatement(at least 1 byte)
	if len(packet) < 6 {
		return nil, errInvalidPacketLength
	}

	// Fifth byte is command
	if packet[4] != comQuery && packet[4] != comStmtPrepare {
		return nil, errInvalidPacketType
	}

	return &QueryRequest{ReadEOFLengthString(packet[5:])}, nil
}

// ComStmtPrepareOkResponse represents COM_STMT_PREPARE_OK response structure.
type ComStmtPrepareOkResponse struct {
	StatementID   uint32 // ID of prepared statement
	ParametersNum uint16 // Num of prepared parameters
}

// DecodeComStmtPrepareOkResponse decodes COM_STMT_PREPARE_OK response from MySQL server.
// Basic packet structure shown below.
// See https://mariadb.com/kb/en/mariadb/com_stmt_prepare/#COM_STMT_PREPARE_OK
//
// int<3> PacketLength
// int<1> PacketNumber
// int<1> Command COM_STMT_PREPARE_OK (0x00)
// int<4> StatementID
// int<2> NumberOfColumns
// int<2> NumberOfParameters
// string<1> <not used>
// int<2> NumberOfWarnings
func DecodeComStmtPrepareOkResponse(packet []byte) (*ComStmtPrepareOkResponse, error) {

	// Min packet length = header(4 bytes) + command(1 byte) + statementID(4 bytes)
	// + number of columns (2 bytes) + number of parameters (2 bytes)
	// + <not used> (1 byte) + number of warnings (2 bytes)
	if len(packet) < 16 {
		return nil, errInvalidPacketLength
	}

	// Fifth byte is command
	if packet[4] != responsePrepareOk {
		return nil, errInvalidPacketType
	}

	statementID := binary.LittleEndian.Uint32(packet[5:9])
	parametersNum := binary.LittleEndian.Uint16(packet[11:13])

	return &ComStmtPrepareOkResponse{StatementID: statementID, ParametersNum: parametersNum}, nil
}

// ComStmtExecuteRequest represents COM_STMT_EXECUTE request structure.
type ComStmtExecuteRequest struct {
	StatementID        uint32              // ID of prepared statement
	PreparedParameters []PreparedParameter // Slice of prepared parameters
}

// PreparedParameter structure represents single prepared parameter structure for COM_STMT_EXECUTE request.
type PreparedParameter struct {
	FieldType byte   // Type of prepared parameter. See https://mariadb.com/kb/en/mariadb/resultset/#field-types
	Flag      byte   // Unused
	Value     string // String value of any prepared parameter passed with COM_STMT_EXECUTE request
}

// DecodeComStmtExecuteRequest decodes COM_STMT_EXECUTE packet sent by MySQL client.
// Basic packet structure shown below.
// See https://mariadb.com/kb/en/mariadb/com_stmt_execute/
//
// int<3> PacketLength
// int<1> PacketNumber
// int<1> COM_STMT_EXECUTE (0x17)
// int<4> StatementID
// int<1> Flags
// int<4> IterationCount = 1
// if (ParamCount > 0)
// {
// 		byte<(ParamCount + 7) / 8> NullBitmap
// 		byte<1>: SendTypeToServer = 0 or 1
// 		if (SendTypeToServer)
//		{
// 			Foreach parameter
//			{
// 				byte<1>: FieldType
//				byte<1>: ParameterFlag
//			}
//		}
// 		Foreach parameter
//		{
// 			byte<n> BinaryParameterValue
//		}
// }
func DecodeComStmtExecuteRequest(packet []byte, paramsCount uint16) (*ComStmtExecuteRequest, error) {

	// Min packet length = header(4 bytes) + command(1 byte) + statementID(4 bytes)
	// + flags(1 byte) + iteration count(4 bytes)
	if len(packet) < 14 {
		return nil, errInvalidPacketLength
	}

	// Fifth byte is command
	if packet[4] != comStmtExecute {
		return nil, errInvalidPacketType
	}

	r := bytes.NewReader(packet)

	// Skip to statementID position
	if _, err := r.Seek(5, io.SeekStart); err != nil {
		return nil, err
	}

	// Read StatementID value
	statementIDBuf := make([]byte, 4)
	if _, err := r.Read(statementIDBuf); err != nil {
		return nil, err
	}
	statementID := binary.LittleEndian.Uint32(statementIDBuf)

	// Skip to NullBitmap position
	if _, err := r.Seek(5, io.SeekCurrent); err != nil {
		return nil, err
	}

	// Prepare buffer for n=paramsCount prepared parameters
	parameters := make([]PreparedParameter, paramsCount)

	if paramsCount > 0 {
		nullBitmapLength := int64((paramsCount + 7) / 8)

		// Skip to SendTypeToServer position
		if _, err := r.Seek(nullBitmapLength, io.SeekCurrent); err != nil {
			return nil, err
		}

		// Read SendTypeToServer value
		sendTypeToServer, err := r.ReadByte()
		if err != nil {
			return nil, err
		}

		if sendTypeToServer == 1 {
			for index := range parameters {

				// Read parameter FieldType and ParameterFlag
				parameterMeta := make([]byte, 2)
				if _, err := r.Read(parameterMeta); err != nil {
					return nil, err
				}

				parameters[index].FieldType = parameterMeta[0]
				parameters[index].Flag = parameterMeta[1]
			}
		}

		var fieldDecoderError error
		var fieldValue string

		for index, parameter := range parameters {
			switch parameter.FieldType {

			// MYSQL_TYPE_VAR_STRING (length encoded string)
			case fieldTypeString:
				fieldValue, fieldDecoderError = DecodeFieldTypeString(r)

			// MYSQL_TYPE_LONGLONG
			case fieldTypeLongLong:
				fieldValue, fieldDecoderError = DecodeFieldTypeLongLong(r)

			// MYSQL_TYPE_DOUBLE
			case fieldTypeDouble:
				fieldValue, fieldDecoderError = DecodeFieldTypeDouble(r)

			// Field with missing decoder
			default:
				return nil, errFieldTypeNotImplementedYet
			}

			// Return with first decoding error
			if fieldDecoderError != nil {
				return nil, fieldDecoderError
			}

			parameters[index].Value = fieldValue
			fieldValue = ""
		}
	}

	return &ComStmtExecuteRequest{StatementID: statementID, PreparedParameters: parameters}, nil
}

// DecodeFieldTypeString decodes MYSQL_TYPE_VAR_STRING field
// See https://mariadb.com/kb/en/mariadb/resultset/#field-types
func DecodeFieldTypeString(r *bytes.Reader) (string, error) {
	var str string

	// Read first byte of parameter value to know buffer length for whole value
	// io.EOF is ok since reader may be empty already because of empty prepared parameter value
	stringLength, err := r.ReadByte()
	if err != nil && err != io.EOF {
		return "", err
	}

	// Put byte back to reader
	if err = r.UnreadByte(); err != nil {
		return "", err
	}

	// Packets with 0-length parameter are also possible
	if stringLength > 0 {
		// Read whole length encoded string
		stringValueBuf := make([]byte, stringLength+1)
		_, err := r.Read(stringValueBuf)
		if err != nil {
			return "", nil
		}

		str, _ = ReadLenEncodedString(stringValueBuf)
	}

	return str, nil
}

// DecodeFieldTypeLongLong decodes MYSQL_TYPE_LONGLONG field
// See https://mariadb.com/kb/en/mariadb/resultset/#field-types
func DecodeFieldTypeLongLong(r *bytes.Reader) (string, error) {
	var bigIntValue int64

	if err := binary.Read(r, binary.LittleEndian, &bigIntValue); err != nil {
		return "", nil
	}

	return strconv.FormatInt(bigIntValue, 10), nil
}

// DecodeFieldTypeDouble decodes MYSQL_TYPE_DOUBLE field
// See https://mariadb.com/kb/en/mariadb/resultset/#field-types
func DecodeFieldTypeDouble(r *bytes.Reader) (string, error) {
	// Read 8 bytes required for float64
	doubleLengthBuf := make([]byte, 8)
	if _, err := r.Read(doubleLengthBuf); err != nil {
		return "", err
	}

	doubleBits := binary.LittleEndian.Uint64(doubleLengthBuf)
	doubleValue := math.Float64frombits(doubleBits)

	return strconv.FormatFloat(doubleValue, 'f', doubleDecodePrecision, 64), nil
}

// ReadLenEncodedInteger returns parsed length-encoded integer and it's offset.
// See https://mariadb.com/kb/en/mariadb/protocol-data-types/#length-encoded-integers
func ReadLenEncodedInteger(data []byte) (value uint64, offset uint64) {
	if len(data) == 0 {
		value = 0
		offset = 0
	}

	switch data[0] {
	case 0xfb:
		value = 0
		offset = 1

	case 0xfc:
		value = uint64(data[1]) | uint64(data[2])<<8
		offset = 3

	case 0xfd:
		value = uint64(data[1]) | uint64(data[2])<<8 | uint64(data[3])<<16
		offset = 4

	case 0xfe:
		value = uint64(data[1]) | uint64(data[2])<<8 | uint64(data[3])<<16 |
			uint64(data[4])<<24 | uint64(data[5])<<32 | uint64(data[6])<<40 |
			uint64(data[7])<<48 | uint64(data[8])<<56
		offset = 9

	default:
		value = uint64(data[0])
		offset = 1
	}

	return value, offset
}

// ReadLenEncodedString returns parsed length-encoded string and it's length.
// Length-encoded strings are prefixed by a length-encoded integer which describes
// the length of the string, followed by the string value.
// See https://mariadb.com/kb/en/mariadb/protocol-data-types/#length-encoded-strings
func ReadLenEncodedString(data []byte) (string, uint64) {
	strLen, offset := ReadLenEncodedInteger(data)

	return string(data[offset : offset+strLen]), strLen
}

// ReadEOFLengthString returns parsed EOF-length string.
// EOF-length strings are those strings whose length will be calculated by the packet remaining length.
// See https://mariadb.com/kb/en/mariadb/protocol-data-types/#end-of-file-length-strings
func ReadEOFLengthString(data []byte) string {
	return string(data)
}

// ReadNullTerminatedString reads bytes from reader until 0x00 byte
// See https://mariadb.com/kb/en/mariadb/protocol-data-types/#null-terminated-strings
func ReadNullTerminatedString(r *bytes.Reader) string {
	var str []byte
	for {
		//TODO: Check for error
		b, _ := r.ReadByte()
		if b == 0x00 {
			return string(str)
		} else {
			str = append(str, b)
		}
	}
}
