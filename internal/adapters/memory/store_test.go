package memory_test

import (
	"testing"

	"github.com/bshadmehr98/candledb/internal/adapters/memory"
	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/app/storetest"
)

func TestStoreContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) app.Store { return memory.NewStore() })
}

func TestUserStoreContract(t *testing.T) {
	storetest.RunUsers(t, func(*testing.T) app.UserStore { return memory.NewUserStore() })
}
