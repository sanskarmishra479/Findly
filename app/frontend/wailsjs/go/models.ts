export namespace main {
	
	export class Info {
	    version: string;
	    hotkeyWarning: string;
	    dataDir: string;
	    indexError: string;
	    modelNote: string;
	
	    static createFrom(source: any = {}) {
	        return new Info(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.hotkeyWarning = source["hotkeyWarning"];
	        this.dataDir = source["dataDir"];
	        this.indexError = source["indexError"];
	        this.modelNote = source["modelNote"];
	    }
	}
	export class PreviewResponse {
	    seq: number;
	    image: string;
	    page: number;
	    pages: number;
	    text: string;
	    err: string;
	
	    static createFrom(source: any = {}) {
	        return new PreviewResponse(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seq = source["seq"];
	        this.image = source["image"];
	        this.page = source["page"];
	        this.pages = source["pages"];
	        this.text = source["text"];
	        this.err = source["err"];
	    }
	}
	export class SearchResult {
	    id: number;
	    itemId: number;
	    name: string;
	    folder: string;
	    kind: string;
	    ext: string;
	    size: number;
	    modified: number;
	    page: number;
	    snippet: string;
	    keyword: boolean;
	    meaning: boolean;
	    exact: boolean;
	    score: number;
	
	    static createFrom(source: any = {}) {
	        return new SearchResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.itemId = source["itemId"];
	        this.name = source["name"];
	        this.folder = source["folder"];
	        this.kind = source["kind"];
	        this.ext = source["ext"];
	        this.size = source["size"];
	        this.modified = source["modified"];
	        this.page = source["page"];
	        this.snippet = source["snippet"];
	        this.keyword = source["keyword"];
	        this.meaning = source["meaning"];
	        this.exact = source["exact"];
	        this.score = source["score"];
	    }
	}
	export class SearchResponse {
	    seq: number;
	    results: SearchResult[];
	    words: string[];
	    meaningOff: string;
	    err: string;
	
	    static createFrom(source: any = {}) {
	        return new SearchResponse(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seq = source["seq"];
	        this.results = this.convertValues(source["results"], SearchResult);
	        this.words = source["words"];
	        this.meaningOff = source["meaningOff"];
	        this.err = source["err"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

