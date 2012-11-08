//  Copyright (c) 2012 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.

package basecouch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"

	"github.com/couchbaselabs/go-couchbase"
	"github.com/dustin/gomemcached"
)

var kDBNameMatch = regexp.MustCompile("[-%+()$_a-z0-9]+")

// Simple error implementation wrapping an HTTP response status.
type HTTPError struct {
	Status  int
	Message string
}

func (err *HTTPError) Error() string {
	return err.Message
}

// Represents a simulated CouchDB database.
type Database struct {
	Name      string `json:"name"`
	DocPrefix string `json:"docPrefix"`
	bucket    *couchbase.Bucket
}

// Helper function to open a Couchbase connection and return a specific bucket.
func ConnectToBucket(couchbaseURL, poolName, bucketName string) (bucket *couchbase.Bucket, err error) {
	bucket, err = couchbase.GetBucket(couchbaseURL, poolName, bucketName)
	if err != nil {
		return
	}
	log.Printf("Connected to <%s>, pool %s, bucket %s", couchbaseURL, poolName, bucketName)
	err = installViews(bucket)
	return
}

// Returns the Couchbase docID of the database's main document
func dbInternalDocName(dbName string) string {
	if !kDBNameMatch.MatchString(dbName) {
		return ""
	}
	return "cdb:" + dbName
}

// Makes a Database object given its name and bucket. Returns nil if there is no such database.
func GetDatabase(bucket *couchbase.Bucket, name string) (*Database, error) {
	docname := dbInternalDocName(name)
	if docname == "" {
		return nil, &HTTPError{Status: 400, Message: "Illegal database name"}
	}
	var db Database
	err := bucket.Get(docname, &db)
	if err != nil {
		return nil, err
	}
	db.bucket = bucket
	return &db, nil
}

// Creates a new database in a bucket and returns a Database object for it. Fails if the database exists.
func CreateDatabase(bucket *couchbase.Bucket, name string) (*Database, error) {
	docname := dbInternalDocName(name)
	if docname == "" {
		return nil, &HTTPError{Status: 400, Message: "Illegal database name"}
	}

	db := Database{bucket: bucket, Name: name, DocPrefix: fmt.Sprintf("doc:%s/%s:", name, createUUID())}
	added, err := bucket.Add(docname, 0, db)
	if err != nil {
		return nil, err
	}
	if !added {
		return nil, &HTTPError{Status: 412, Message: "Database already exists"}
	}
	return &db, nil
}

func (db *Database) SameAs(otherdb *Database) bool {
	return db != nil && otherdb != nil &&
		db.bucket == otherdb.bucket && db.DocPrefix == otherdb.DocPrefix
}

// The UUID assigned to this database.
func (db *Database) UUID() string {
	return db.DocPrefix[4 : len(db.DocPrefix)-1]
}

//////// ALL DOCUMENTS:

// The number of documents in the database.
func (db *Database) DocCount() int {
	vres, err := db.queryAllDocs(true)
	if err != nil {
		return -1
	}
	if len(vres.Rows) == 0 {
		return 0
	}
	return int(vres.Rows[0].Value.(float64))
}

func installViews(bucket *couchbase.Bucket) error {
	node := bucket.Nodes[rand.Intn(len(bucket.Nodes))]
	u, err := url.Parse(node.CouchAPIBase)
	if err != nil {
		fmt.Printf("Failed to parse %s", node.CouchAPIBase)
		return err
	}
	u.Path = fmt.Sprintf("/%s/_design/%s", bucket.Name, "couchdb")

	// View for _all_dbs
	alldbs_map := `function (doc, meta) {
                     var pieces = meta.id.split(":");
                     if (pieces.length != 2 || pieces[0] != "cdb")
                        return;
                     emit(doc.name, doc.docPrefix); }`
	// View for finding every Couchbase doc used by a database (for cleanup upon deletion)
	allbits_map := `function (doc, meta) {
                     var pieces = meta.id.split(":", 3);
                     if (pieces.length < 2)
                       return;
					 var type = pieces[0];
					 if (type == "doc" || type == "ldoc" || type == "seq")
                       emit(pieces[1], null); }`
	// View for _all_docs
	alldocs_map := `function (doc, meta) {
                     var pieces = meta.id.split(":", 3);
                     if (pieces.length < 2 || pieces[0] != "doc")
                       return;
                     if (doc.deleted || doc.id===undefined)
                       return;
                     emit([pieces[1], doc.id], doc.rev); }`
	// View for _changes feed, i.e. a by-sequence index
	changes_map := `function (doc, meta) {
                    if (doc.sequence === undefined)
                        return;
                    var pieces = meta.id.split(":", 3);
                    if (pieces.length < 3 || pieces[0] != "doc" || doc.id===undefined)
                        return;
                    var value = [doc.id, doc.rev];
                    if (doc.deleted)
                        value.push(true)
                    emit([pieces[1], doc.sequence], value); }`
	// View for mapping revision IDs to documents (for vacuuming)
	revs_map := `function (doc, meta) {
					  var pieces = meta.id.split(":", 3);
					  if (pieces.length < 2)
					    return;
					  if (pieces[0] == "rev") {
					    emit(pieces[1], true);
					  } else if (pieces[0] == "doc") {
					    var keys = doc.history.keys;
					    for (var i = 0; i < keys.length; ++i) {
					      if (keys[i] != "")
					        emit(keys[i], false);
					    }
					  }
					}`
	// View for mapping attachment IDs to revisions (for vacuuming)
	atts_map := `function (doc, meta) {
				  var pieces = meta.id.split(":", 3);
				  if (pieces.length < 2)
				    return;
				  if (pieces[0] == "att") {
				    emit(pieces[1], true);
				  } else if (pieces[0] == "rev") {
				    var atts = doc._attachments;
				    if (atts) {
				      for (var i = 0; i < atts.length; ++i) {
				        var digest = atts[i].digest
				        if (digest)
				          emit(digest, false);
				      }
				    }
				  }
				}`
	// Reduce function for revs_map and atts_map
	revs_or_atts_reduce := `function(keys, values, rereduce) {
							  for (var i = 0; i < values.length; ++i) {
							    if (!values[i])
							      return false;
							  }
							  return true;
							}`

	ddoc := Body{
		"language": "javascript",
		"views": Body{
			"all_dbs":  Body{"map": alldbs_map},
			"all_bits": Body{"map": allbits_map},
			"all_docs": Body{"map": alldocs_map, "reduce": "_count"},
			"revs":     Body{"map": revs_map, "reduce": revs_or_atts_reduce},
			"atts":     Body{"map": atts_map, "reduce": revs_or_atts_reduce},
			"changes":  Body{"map": changes_map}}}
	payload, err := json.Marshal(ddoc)
	rq, err := http.NewRequest("PUT", u.String(), bytes.NewBuffer(payload))
	rq.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(rq)

	if err == nil && response.StatusCode > 299 {
		err = &HTTPError{Status: response.StatusCode, Message: response.Status}
	}
	if err == nil {
		log.Printf("Installed design doc <%s>", u)
	} else {
		log.Printf("WARNING: Error installing design doc: %v", err)
	}
	return err
}

// Returns all database names as an array.
func AllDbNames(bucket *couchbase.Bucket) ([]string, error) {
	vres, err := bucket.View("couchdb", "all_dbs", Body{"stale": false})
	if err != nil {
		log.Printf("WARNING: View returned %v", err)
		return nil, err
	}

	rows := vres.Rows
	result := make([]string, 0, len(rows))
	for _, row := range rows {
		result = append(result, row.Key.(string))
	}
	return result, nil
}

type IDAndRev struct {
	DocID string
	RevID string
}

// Returns all document IDs as an array.
func (db *Database) AllDocIDs() ([]IDAndRev, error) {
	vres, err := db.queryAllDocs(false)
	if err != nil {
		return nil, err
	}

	rows := vres.Rows
	result := make([]IDAndRev, 0, len(rows))
	for _, row := range rows {
		key := row.Key.([]interface{})
		result = append(result, IDAndRev{DocID: key[1].(string), RevID: row.Value.(string)})
	}
	return result, nil
}

func (db *Database) queryAllDocs(reduce bool) (couchbase.ViewResult, error) {
	uuid := db.UUID()
	startkey := [1]string{uuid}
	endkey := [2]interface{}{uuid, make(Body)}
	opts := Body{"stale": false, "startkey": startkey, "endkey": endkey, "reduce": reduce}
	vres, err := db.bucket.View("couchdb", "all_docs", opts)
	if err != nil {
		log.Printf("WARNING: all_docs got error: %v", err)
	}
	return vres, err
}

// Deletes a database (and all documents)
func (db *Database) Delete() error {
	uuid := db.UUID()
	opts := Body{"stale": false, "startkey": uuid, "endkey": uuid}
	vres, err := db.bucket.View("couchdb", "all_bits", opts)
	if err != nil {
		log.Printf("WARNING: all_bits view returned %v", err)
		return err
	}

	// First delete the database doc itself. At this point the db is officially gone:
	err = db.bucket.Delete(dbInternalDocName(db.Name))
	if err != nil {
		return err
	}

	// Now delete all of its docs:
	//FIX: Is there a way to do this in one operation?
	log.Printf("Deleting %d documents of %q ...", len(vres.Rows), db.Name)
	for _, row := range vres.Rows {
		if LogRequestsVerbose {
			log.Printf("\tDeleting %q", row.ID)
		}
		if err := db.bucket.Delete(row.ID); err != nil {
			log.Printf("WARNING: Error deleting %q: %v", row.ID, err)
		}
	}
	return nil
}

// Deletes all orphaned CouchDB documents without a database.
// This is necessary because the views are not usually up to date with the database, so at the
// time db.Delete runs, it may not have all the document IDs, causing documents to be left behind.
func VacuumDocs(bucket *couchbase.Bucket) (int, error) {
	// First collect all database UUIDs:
	vres, err := bucket.View("couchdb", "all_dbs", Body{"stale": false})
	if err != nil {
		log.Printf("WARNING: all_dbs view returned %v (%v)", err, vres)
		return 0, err
	}
	uuids := map[string]bool{}
	for _, row := range vres.Rows {
		docPrefix := row.Value.(string)
		uuids[docPrefix[4:len(docPrefix)-1]] = true
	}

	// Now go through the entire all_bits view and delete docs without a db:
	vres, err = bucket.View("couchdb", "all_bits", Body{"stale": false})
	if err != nil {
		log.Printf("WARNING: all_bits view returned %v", err)
		return 0, err
	}

	deletions := 0
	for _, row := range vres.Rows {
		dbid := row.Key.(string)
		if !uuids[dbid] {
			deletions += vacIt(bucket, row.ID)
		}
	}
	return deletions, nil
}

func vacuumContent(bucket *couchbase.Bucket, viewName string, docPrefix string) (int, error) {
	vres, err := bucket.View("couchdb", viewName, Body{"stale": false, "group_level": 1})
	if err != nil {
		log.Printf("WARNING: %s view returned %v", viewName, err)
		return 0, err
	}
	deletions := 0
	for _, row := range vres.Rows {
		if row.Value.(bool) {
			deletions += vacIt(bucket, docPrefix+row.Key.(string))
		}
	}
	return deletions, nil
}

func vacIt(bucket *couchbase.Bucket, docid string) int {
	if err := bucket.Delete(docid); err != nil {
		log.Printf("\tError vacuuming %q: %v", docid, err)
		return 0
	}
	if LogRequestsVerbose {
		log.Printf("\tVacuumed %q", docid)
	}
	return 1
}

// Deletes all orphaned CouchDB revisions not used by any documents.
func VacuumRevisions(bucket *couchbase.Bucket) (int, error) {
	return vacuumContent(bucket, "revs", "rev:")
}

// Deletes all orphaned CouchDB attachments not used by any revisions.
func VacuumAttachments(bucket *couchbase.Bucket) (int, error) {
	return vacuumContent(bucket, "atts", "att:")
}

//////// SEQUENCES & CHANGES:

func (db *Database) sequenceDocID() string {
	return "seq:" + db.UUID()
}

func (db *Database) LastSequence() (uint64, error) {
	return db.bucket.Incr(db.sequenceDocID(), 0, 0, 0)
}

func (db *Database) generateSequence() (uint64, error) {
	return db.bucket.Incr(db.sequenceDocID(), 1, 1, 0)
}

// Options for Database.getChanges
type ChangesOptions struct {
	Since       uint64
	Limit       int
	Conflicts   bool
	IncludeDocs bool
	includeDocMeta bool
	Wait        bool
}

// A changes entry; Database.getChanges returns an array of these.
// Marshals into the standard CouchDB _changes format.
type ChangeEntry struct {
	Seq     uint64      `json:"seq"`
	ID      string      `json:"id"`
	Deleted bool        `json:"deleted,omitempty"`
	Doc     Body        `json:"doc,omitempty"`
	docMeta *document
	Changes []ChangeRev `json:"changes"`
}

type ChangeRev map[string]string

type ViewDoc struct {
	Json json.RawMessage	// should be type 'document', but that fails to unmarshal correctly
}

type ViewRow struct {
	ID    string
	Key   interface{}
	Value interface{}
	Doc   *ViewDoc
}

type ViewResult struct {
	TotalRows int `json:"total_rows"`
	Rows      []ViewRow
	Errors    []couchbase.ViewError
}

const kChangesPageSize = 200

// Returns a list of all the changes made to the database, a la the _changes feed.
func (db *Database) ChangesFeed(options ChangesOptions) (<-chan *ChangeEntry, error) {
	// http://wiki.apache.org/couchdb/HTTP_database_API#Changes
	uuid := db.UUID()
	lastSequence := options.Since
	endkey := [2]interface{}{uuid, make(Body)}
	totalLimit := options.Limit
	usingDocs := options.Conflicts || options.IncludeDocs || options.includeDocMeta
	opts := Body{"stale": false, "update_seq": true,
		"endkey": endkey,
		"include_docs": usingDocs}

	feed := make(chan *ChangeEntry, kChangesPageSize)
	
	lastSeq,err := db.LastSequence()
	if options.Since >= lastSeq && err == nil {
		close(feed)
		return feed, nil
	}

	// Generate the output in a new goroutine, writing to 'feed':
	go func() {
		defer close(feed)
		for {
			// Query the 'changes' view:
			opts["startkey"] = [2]interface{}{uuid, lastSequence + 1}
			limit := totalLimit
			if limit == 0 || limit > kChangesPageSize {
				limit = kChangesPageSize
			}
			opts["limit"] = limit
			
			var vres ViewResult
			var err error
			for len(vres.Rows) == 0 {
				vres = ViewResult{}
				err = db.bucket.ViewCustom("couchdb", "changes", opts, &vres)
				if err != nil {
					log.Printf("Error from 'changes' view: %v", err)
					return
				}
				if len(vres.Rows) == 0 {
					if !options.Wait || !db.WaitForRevision() {
						return
					}
				}
			}

			for _, row := range vres.Rows {
				key := row.Key.([]interface{})
				lastSequence = uint64(key[1].(float64))
				value := row.Value.([]interface{})
				docID := value[0].(string)
				revID := value[1].(string)
				entry := &ChangeEntry{
					Seq:     lastSequence,
					ID:      docID,
					Changes: []ChangeRev{{"rev": revID}},
					Deleted: (len(value) >= 3 && value[2].(bool)),
				}
				if usingDocs {
					doc := newDocument()
					json.Unmarshal(row.Doc.Json, doc)
					if doc != nil {
						//log.Printf("?? doc = %v", doc)
						if options.Conflicts {
							for _, leafID := range doc.History.getLeaves() {
								if leafID != revID {
									entry.Changes = append(entry.Changes, ChangeRev{"rev": leafID})
								}
							}
						}
						if options.IncludeDocs {
							key := doc.History[revID].Key
							if key != "" {
								entry.Doc, _ = db.getRevFromDoc(doc, revID, false)
							}
						}
						if options.includeDocMeta {
							entry.docMeta = doc
						}
					}
				}
				feed <- entry
			}
			
			// Step to the next page of results:
			nRows := len(vres.Rows)
			if nRows < kChangesPageSize || options.Wait {
				break
			}
			if totalLimit > 0 {
				totalLimit -= nRows
				if totalLimit <= 0 {
					break
				}
			}
			delete(opts,"stale") // we only need to update the index once
		}
	}()
	return feed, nil
}

func (db *Database) GetChanges(options ChangesOptions) ([]*ChangeEntry, error) {
	var changes []*ChangeEntry
	feed, err := db.ChangesFeed(options)
	if err == nil && feed != nil {
		changes = make([]*ChangeEntry, 0, 50)
		for entry := range feed {
			changes = append(changes, entry)
		}
	}
	return changes, err
}

func (db *Database) WaitForRevision() bool {
	log.Printf("\twaiting for a revision...")
	waitFor(db.DocPrefix)
	log.Printf("\t...done waiting")
	return true
}

func (db *Database) NotifyRevision() {
	notify(db.DocPrefix)
}

//////// UTILITIES:

// Attempts to map an error to an HTTP status code and message.
// Defaults to 500 if it doesn't recognize the error. Returns 200 for a nil error.
func ErrorAsHTTPStatus(err error) (int, string) {
	if err == nil {
		return 200, "OK"
	}
	switch err := err.(type) {
	case *HTTPError:
		return err.Status, err.Message
	case *gomemcached.MCResponse:
		switch err.Status {
		case gomemcached.KEY_ENOENT:
			return http.StatusNotFound, "missing"
		case gomemcached.KEY_EEXISTS:
			return http.StatusConflict, "Conflict"
		case gomemcached.E2BIG:
			return http.StatusRequestEntityTooLarge, "Too Large"
		default:
			return http.StatusBadGateway, fmt.Sprintf("MC status %d", err.Status)
		}
	default:
		log.Printf("WARNING: Couldn't interpret error type %T, value %v", err, err)
		return http.StatusInternalServerError, fmt.Sprintf("Internal error: %v", err)
	}
	panic("unreachable")
}
