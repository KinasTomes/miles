package dynamodb

import (
	"encoding/json"
	"fmt"

	s "github.com/KinasTomes/miles/internal/storage"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// MaxWireBytes is a conservative application transport budget, NOT the AWS
// native 4 MiB item aggregate limit. For this builder's supported S/N/B/BOOL/M/L
// attributes, Go's JSON representation includes larger union field names and
// unused nil action fields. It bounds the SDK JSON wire size from above; native
// item limits still use ItemSizeUpperBound, not this serialization.
const MaxWireBytes = 8 << 20

func validateWireBudget(input *ddb.TransactWriteItemsInput) error {
	body, err := json.Marshal(input)
	if err != nil || len(body) > MaxWireBytes {
		return fmt.Errorf("%w: transaction transport budget", s.ErrInvalid)
	}
	return nil
}
