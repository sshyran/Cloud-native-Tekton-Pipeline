// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.27.1
// 	protoc        v3.20.1
// source: spire/api/types/x509svid.proto

package types

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

// X.509 SPIFFE Verifiable Identity Document. It contains the raw X.509
// certificate data as well as a few denormalized fields for convenience.
type X509SVID struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	// Certificate and intermediates required to form a chain of trust back to
	// the X.509 authorities of the trust domain (ASN.1 DER encoded).
	CertChain [][]byte `protobuf:"bytes,1,rep,name=cert_chain,json=certChain,proto3" json:"cert_chain,omitempty"`
	// SPIFFE ID of the SVID.
	Id *SPIFFEID `protobuf:"bytes,2,opt,name=id,proto3" json:"id,omitempty"`
	// Expiration timestamp (seconds since Unix epoch).
	ExpiresAt int64 `protobuf:"varint,3,opt,name=expires_at,json=expiresAt,proto3" json:"expires_at,omitempty"`
}

func (x *X509SVID) Reset() {
	*x = X509SVID{}
	if protoimpl.UnsafeEnabled {
		mi := &file_spire_api_types_x509svid_proto_msgTypes[0]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *X509SVID) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*X509SVID) ProtoMessage() {}

func (x *X509SVID) ProtoReflect() protoreflect.Message {
	mi := &file_spire_api_types_x509svid_proto_msgTypes[0]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use X509SVID.ProtoReflect.Descriptor instead.
func (*X509SVID) Descriptor() ([]byte, []int) {
	return file_spire_api_types_x509svid_proto_rawDescGZIP(), []int{0}
}

func (x *X509SVID) GetCertChain() [][]byte {
	if x != nil {
		return x.CertChain
	}
	return nil
}

func (x *X509SVID) GetId() *SPIFFEID {
	if x != nil {
		return x.Id
	}
	return nil
}

func (x *X509SVID) GetExpiresAt() int64 {
	if x != nil {
		return x.ExpiresAt
	}
	return 0
}

var File_spire_api_types_x509svid_proto protoreflect.FileDescriptor

var file_spire_api_types_x509svid_proto_rawDesc = []byte{
	0x0a, 0x1e, 0x73, 0x70, 0x69, 0x72, 0x65, 0x2f, 0x61, 0x70, 0x69, 0x2f, 0x74, 0x79, 0x70, 0x65,
	0x73, 0x2f, 0x78, 0x35, 0x30, 0x39, 0x73, 0x76, 0x69, 0x64, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f,
	0x12, 0x0f, 0x73, 0x70, 0x69, 0x72, 0x65, 0x2e, 0x61, 0x70, 0x69, 0x2e, 0x74, 0x79, 0x70, 0x65,
	0x73, 0x1a, 0x1e, 0x73, 0x70, 0x69, 0x72, 0x65, 0x2f, 0x61, 0x70, 0x69, 0x2f, 0x74, 0x79, 0x70,
	0x65, 0x73, 0x2f, 0x73, 0x70, 0x69, 0x66, 0x66, 0x65, 0x69, 0x64, 0x2e, 0x70, 0x72, 0x6f, 0x74,
	0x6f, 0x22, 0x73, 0x0a, 0x08, 0x58, 0x35, 0x30, 0x39, 0x53, 0x56, 0x49, 0x44, 0x12, 0x1d, 0x0a,
	0x0a, 0x63, 0x65, 0x72, 0x74, 0x5f, 0x63, 0x68, 0x61, 0x69, 0x6e, 0x18, 0x01, 0x20, 0x03, 0x28,
	0x0c, 0x52, 0x09, 0x63, 0x65, 0x72, 0x74, 0x43, 0x68, 0x61, 0x69, 0x6e, 0x12, 0x29, 0x0a, 0x02,
	0x69, 0x64, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x19, 0x2e, 0x73, 0x70, 0x69, 0x72, 0x65,
	0x2e, 0x61, 0x70, 0x69, 0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x53, 0x50, 0x49, 0x46, 0x46,
	0x45, 0x49, 0x44, 0x52, 0x02, 0x69, 0x64, 0x12, 0x1d, 0x0a, 0x0a, 0x65, 0x78, 0x70, 0x69, 0x72,
	0x65, 0x73, 0x5f, 0x61, 0x74, 0x18, 0x03, 0x20, 0x01, 0x28, 0x03, 0x52, 0x09, 0x65, 0x78, 0x70,
	0x69, 0x72, 0x65, 0x73, 0x41, 0x74, 0x42, 0x37, 0x5a, 0x35, 0x67, 0x69, 0x74, 0x68, 0x75, 0x62,
	0x2e, 0x63, 0x6f, 0x6d, 0x2f, 0x73, 0x70, 0x69, 0x66, 0x66, 0x65, 0x2f, 0x73, 0x70, 0x69, 0x72,
	0x65, 0x2d, 0x61, 0x70, 0x69, 0x2d, 0x73, 0x64, 0x6b, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x2f,
	0x73, 0x70, 0x69, 0x72, 0x65, 0x2f, 0x61, 0x70, 0x69, 0x2f, 0x74, 0x79, 0x70, 0x65, 0x73, 0x62,
	0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_spire_api_types_x509svid_proto_rawDescOnce sync.Once
	file_spire_api_types_x509svid_proto_rawDescData = file_spire_api_types_x509svid_proto_rawDesc
)

func file_spire_api_types_x509svid_proto_rawDescGZIP() []byte {
	file_spire_api_types_x509svid_proto_rawDescOnce.Do(func() {
		file_spire_api_types_x509svid_proto_rawDescData = protoimpl.X.CompressGZIP(file_spire_api_types_x509svid_proto_rawDescData)
	})
	return file_spire_api_types_x509svid_proto_rawDescData
}

var file_spire_api_types_x509svid_proto_msgTypes = make([]protoimpl.MessageInfo, 1)
var file_spire_api_types_x509svid_proto_goTypes = []interface{}{
	(*X509SVID)(nil), // 0: spire.api.types.X509SVID
	(*SPIFFEID)(nil), // 1: spire.api.types.SPIFFEID
}
var file_spire_api_types_x509svid_proto_depIdxs = []int32{
	1, // 0: spire.api.types.X509SVID.id:type_name -> spire.api.types.SPIFFEID
	1, // [1:1] is the sub-list for method output_type
	1, // [1:1] is the sub-list for method input_type
	1, // [1:1] is the sub-list for extension type_name
	1, // [1:1] is the sub-list for extension extendee
	0, // [0:1] is the sub-list for field type_name
}

func init() { file_spire_api_types_x509svid_proto_init() }
func file_spire_api_types_x509svid_proto_init() {
	if File_spire_api_types_x509svid_proto != nil {
		return
	}
	file_spire_api_types_spiffeid_proto_init()
	if !protoimpl.UnsafeEnabled {
		file_spire_api_types_x509svid_proto_msgTypes[0].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*X509SVID); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_spire_api_types_x509svid_proto_rawDesc,
			NumEnums:      0,
			NumMessages:   1,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_spire_api_types_x509svid_proto_goTypes,
		DependencyIndexes: file_spire_api_types_x509svid_proto_depIdxs,
		MessageInfos:      file_spire_api_types_x509svid_proto_msgTypes,
	}.Build()
	File_spire_api_types_x509svid_proto = out.File
	file_spire_api_types_x509svid_proto_rawDesc = nil
	file_spire_api_types_x509svid_proto_goTypes = nil
	file_spire_api_types_x509svid_proto_depIdxs = nil
}
