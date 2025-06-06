package encoding

type Decoder interface {
	// Decode takes in the chunks, indices, and encoding parameters and returns the decoded blob
	Decode(chunks []*Frame, indices []ChunkNumber, params EncodingParams, inputSize uint64) ([]byte, error)
}

type Prover interface {
	Decoder
	// Encode takes in a blob and returns the commitments and encoded chunks. The encoding will satisfy the property that
	// for any number M such that M*params.ChunkLength > BlobCommitments.Length, then any set of M chunks will be sufficient to
	// reconstruct the blob.
	EncodeAndProve(data []byte, params EncodingParams) (BlobCommitments, []*Frame, error)

	// GetCommitmentsForPaddedLength takes in a byte slice representing a list of bn254
	// field elements (32 bytes each, except potentially the last element),
	// pads the (potentially incomplete) last element with zeroes, and returns the commitments for the padded list.
	GetCommitmentsForPaddedLength(data []byte) (BlobCommitments, error)

	GetFrames(data []byte, params EncodingParams) ([]*Frame, error)

	GetMultiFrameProofs(data []byte, params EncodingParams) ([]Proof, error)

	GetSRSOrder() uint64
}

type Verifier interface {
	Decoder

	// VerifyChunks takes in the chunks, indices, commitments, and encoding parameters and returns an error if the chunks are invalid.
	VerifyFrames(chunks []*Frame, indices []ChunkNumber, commitments BlobCommitments, params EncodingParams) error

	// VerifyBatch takes in the encoding parameters, samples and the number of blobs and returns an error if a chunk in any sample is invalid.
	UniversalVerifySubBatch(params EncodingParams, samples []Sample, numBlobs int) error

	// VerifyBlobLength takes in the commitments and returns an error if the blob length is invalid.
	VerifyBlobLength(commitments BlobCommitments) error

	// VerifyCommitEquivalence takes in a list of commitments and returns an error if the commitment of G1 and G2 are inconsistent
	VerifyCommitEquivalenceBatch(commitments []BlobCommitments) error
}
