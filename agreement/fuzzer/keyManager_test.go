// Copyright (C) 2019 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package fuzzer

import (
	"github.com/vincentbdb/go-algorand/data/account"
	"github.com/vincentbdb/go-algorand/data/basics"
)

type simpleKeyManager []account.Participation

func (m simpleKeyManager) Keys() []account.Participation {
	return m
}

func (m simpleKeyManager) HasLiveKeys(from, to basics.Round) bool {
	for _, acc := range m {
		if acc.OverlapsInterval(from, to) {
			return true
		}
	}
	return false
}
