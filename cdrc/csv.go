/*
Real-time Online/Offline Charging System (OCS) for Telecom & ISP environments
Copyright (C) ITsysCOM GmbH

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>
*/

package cdrc

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/cgrates/cgrates/config"
	"github.com/cgrates/cgrates/engine"
	"github.com/cgrates/cgrates/utils"
)

func NewCsvRecordsProcessor(csvReader *csv.Reader, timezone, fileName string,
	dfltCdrcCfg *config.CdrcCfg, cdrcCfgs []*config.CdrcCfg,
	httpSkipTlsCheck bool, cacheDumpFields []*config.FCTemplate,
	filterS *engine.FilterS,
	unp *UnpairedRecordsCache, prt *PartialRecordsCache,
	connMgr *engine.ConnManager) *CsvRecordsProcessor {
	return &CsvRecordsProcessor{csvReader: csvReader,
		timezone: timezone, fileName: fileName,
		dfltCdrcCfg: dfltCdrcCfg, cdrcCfgs: cdrcCfgs,
		httpSkipTlsCheck:       httpSkipTlsCheck,
		unpairedRecordsCache:   unp,
		partialRecordsCache:    prt,
		partialCacheDumpFields: cacheDumpFields, filterS: filterS,
		connMgr: connMgr}

}

type CsvRecordsProcessor struct {
	csvReader              *csv.Reader
	timezone               string // Timezone for CDRs which are not clearly specifying it
	fileName               string
	dfltCdrcCfg            *config.CdrcCfg
	cdrcCfgs               []*config.CdrcCfg
	processedRecordsNr     int64 // Number of content records in file
	httpSkipTlsCheck       bool
	unpairedRecordsCache   *UnpairedRecordsCache // Shared by cdrc so we can cache for all files in a folder
	partialRecordsCache    *PartialRecordsCache  // Cache records which are of type "Partial"
	partialCacheDumpFields []*config.FCTemplate
	filterS                *engine.FilterS
	connMgr                *engine.ConnManager
}

func (self *CsvRecordsProcessor) ProcessedRecordsNr() int64 {
	return self.processedRecordsNr
}

func (self *CsvRecordsProcessor) ProcessNextRecord() ([]*engine.CDR, error) {
	record, err := self.csvReader.Read()
	if err != nil {
		return nil, err
	}
	self.processedRecordsNr += 1
	if utils.SliceHasMember([]string{utils.MetaKamFlatstore, utils.MetaOsipsFlatstore}, self.dfltCdrcCfg.CdrFormat) {
		if record, err = self.processFlatstoreRecord(record); err != nil {
			return nil, err
		} else if record == nil {
			return nil, nil // Due to partial, none returned
		}
	}
	// Record was overwriten with complete information out of cache
	return self.processRecord(record)
}

// Processes a single partial record for flatstore CDRs
func (self *CsvRecordsProcessor) processFlatstoreRecord(record []string) ([]string, error) {
	if strings.HasPrefix(self.fileName, self.dfltCdrcCfg.FailedCallsPrefix) { // Use the first index since they should be the same in all configs
		record = append(record, "0") // Append duration 0 for failed calls flatstore CDR and do not process it further
		return record, nil
	}
	pr, err := NewUnpairedRecord(record, self.timezone)
	if err != nil {
		return nil, err
	}
	// Retrieve and complete the record from cache
	cachedFilename, cachedPartial := self.unpairedRecordsCache.GetPartialRecord(pr.OriginID, self.fileName)
	if cachedPartial == nil { // Not cached, do it here and stop processing
		self.unpairedRecordsCache.CachePartial(self.fileName, pr)
		return nil, nil
	}
	pairedRecord, err := pairToRecord(cachedPartial, pr)
	if err != nil {
		return nil, err
	}
	self.unpairedRecordsCache.UncachePartial(cachedFilename, pr)
	return pairedRecord, nil
}

// Takes the record from a slice and turns it into StoredCdrs, posting them to the cdrServer
func (self *CsvRecordsProcessor) processRecord(record []string) ([]*engine.CDR, error) {
	csvProvider := config.NewSliceDP(record, utils.MetaReq)
	recordCdrs := make([]*engine.CDR, 0)    // More CDRs based on the number of filters and field templates
	for _, cdrcCfg := range self.cdrcCfgs { // cdrFields coming from more templates will produce individual storCdr records
		tenant, err := cdrcCfg.Tenant.ParseDataProvider(csvProvider, utils.NestingSep) // each profile of cdrc can have different tenant
		if err != nil {
			return nil, err
		}
		if tenant == "" {
			tenant = config.CgrConfig().GeneralCfg().DefaultTenant
		}
		// Make sure filters are matching
		if len(cdrcCfg.Filters) != 0 {
			if pass, err := self.filterS.Pass(tenant,
				cdrcCfg.Filters, csvProvider); err != nil || !pass {
				continue // Not passes filters, ignore this CDR
			}
		}
		storedCdr, err := self.recordToStoredCdr(record, cdrcCfg, tenant)
		if err != nil {
			return nil, fmt.Errorf("Failed converting to StoredCdr, error: %s", err.Error())
		} else if self.dfltCdrcCfg.CdrFormat == utils.MetaPartialCSV {
			if storedCdr, err = self.partialRecordsCache.MergePartialCDRRecord(NewPartialCDRRecord(storedCdr, self.partialCacheDumpFields)); err != nil {
				return nil, fmt.Errorf("Failed merging PartialCDR, error: %s", err.Error())
			} else if storedCdr == nil { // CDR was absorbed by cache since it was partial
				continue
			}
		}
		recordCdrs = append(recordCdrs, storedCdr)
		if !cdrcCfg.ContinueOnSuccess {
			break
		}
	}
	return recordCdrs, nil
}

// Takes the record out of csv and turns it into storedCdr which can be processed by CDRS
func (self *CsvRecordsProcessor) recordToStoredCdr(record []string, cdrcCfg *config.CdrcCfg, tenant string) (*engine.CDR, error) {
	storedCdr := &engine.CDR{OriginHost: "0.0.0.0", Source: cdrcCfg.CdrSourceId, ExtraFields: make(map[string]string), Cost: -1}
	var err error
	csvProvider := config.NewSliceDP(record, utils.MetaReq) // used for filterS and for RSRParsers
	var lazyHttpFields []*config.FCTemplate
	fldVals := make(map[string]string)
	for _, cdrFldCfg := range cdrcCfg.ContentFields {
		if len(cdrFldCfg.Filters) != 0 {
			if pass, err := self.filterS.Pass(tenant,
				cdrFldCfg.Filters, csvProvider); err != nil {
				return nil, err
			} else if !pass {
				continue
			}
		}
		if utils.SliceHasMember([]string{utils.MetaKamFlatstore, utils.MetaOsipsFlatstore}, self.dfltCdrcCfg.CdrFormat) { // Hardcode some values in case of flatstore
			switch cdrFldCfg.FieldId {
			case utils.OriginID:
				cdrFldCfg.Value = config.NewRSRParsersMustCompile("~*req.3;~*req.1;~*req.2", true, utils.INFIELD_SEP) // in case of flatstore, accounting id is made up out of callid, from_tag and to_tag
			case utils.Usage:
				cdrFldCfg.Value = config.NewRSRParsersMustCompile("~*req."+strconv.Itoa(len(record)-1), true, utils.INFIELD_SEP) // in case of flatstore, last element will be the duration computed by us
			}
		}
		switch cdrFldCfg.Type {
		case utils.META_COMPOSED:
			out, err := cdrFldCfg.Value.ParseDataProvider(csvProvider, utils.NestingSep)
			if err != nil {
				return nil, err
			}
			fldVals[cdrFldCfg.FieldId] += out
		case utils.MetaUnixTimestamp:
			out, err := cdrFldCfg.Value.ParseDataProvider(csvProvider, utils.NestingSep)
			if err != nil {
				return nil, err
			}
			t, err := utils.ParseTimeDetectLayout(out, self.timezone)
			if err != nil {
				return nil, err
			}
			fldVals[cdrFldCfg.FieldId] += strconv.Itoa(int(t.Unix()))
		case utils.META_HTTP_POST:
			lazyHttpFields = append(lazyHttpFields, cdrFldCfg) // Will process later so we can send an estimation of storedCdr to http server
		default:
			return nil, fmt.Errorf("Unsupported field type: %s", cdrFldCfg.Type)
		}
		if err := storedCdr.ParseFieldValue(cdrFldCfg.FieldId, fldVals[cdrFldCfg.FieldId], self.timezone); err != nil {
			return nil, err
		}
	}
	storedCdr.CGRID = utils.Sha1(storedCdr.OriginID, storedCdr.OriginHost)
	for _, httpFieldCfg := range lazyHttpFields { // Lazy process the http fields
		var outValByte []byte
		var fieldVal, httpAddr string
		for _, rsrFld := range httpFieldCfg.Value {
			if parsed, err := rsrFld.ParseValue(utils.EmptyString); err != nil {
				return nil, fmt.Errorf("Ignoring record: %v - cannot extract http address for field %+v, err: %s",
					record, rsrFld, err.Error())
			} else {
				httpAddr += parsed
			}
		}
		var jsn []byte
		jsn, err = json.Marshal(storedCdr)
		if err != nil {
			return nil, err
		}
		if outValByte, err = engine.HttpJsonPost(httpAddr, self.httpSkipTlsCheck, jsn); err != nil && httpFieldCfg.Mandatory {
			return nil, err
		} else {
			fieldVal = string(outValByte)
			if len(fieldVal) == 0 && httpFieldCfg.Mandatory {
				return nil, fmt.Errorf("MandatoryIeMissing: Empty result for http_post field: %s", httpFieldCfg.Tag)
			}
			if err := storedCdr.ParseFieldValue(httpFieldCfg.FieldId, fieldVal, self.timezone); err != nil {
				return nil, err
			}
		}
	}
	return storedCdr, nil
}
